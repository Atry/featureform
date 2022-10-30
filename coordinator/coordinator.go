package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	help "github.com/featureform/helpers"
	"github.com/featureform/types"

	db "github.com/jackc/pgx/v4"
	"go.uber.org/zap"

	"github.com/featureform/kubernetes"
	"github.com/featureform/metadata"
	"github.com/featureform/provider"
	"github.com/featureform/runner"
	mvccpb "go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

func retryWithDelays(name string, retries int, delay time.Duration, idempotentFunction func() error) error {
	var err error
	for i := 0; i < retries; i++ {
		if err = idempotentFunction(); err == nil {
			return nil
		}
		time.Sleep(delay)
	}
	return fmt.Errorf("retried %s %d times unsuccesfully: Latest error message: %v", name, retries, err)
}

type Config []byte

func templateReplace(template string, replacements map[string]string, offlineStore provider.OfflineStore) (string, error) {
	formattedString := ""
	numEscapes := strings.Count(template, "{{")
	for i := 0; i < numEscapes; i++ {
		split := strings.SplitN(template, "{{", 2)
		afterSplit := strings.SplitN(split[1], "}}", 2)
		key := strings.TrimSpace(afterSplit[0])
		replacement, has := replacements[key]
		if !has {
			return "", fmt.Errorf("no key set")
		}

		if offlineStore.Type() == provider.BigQueryOffline {
			bqConfig := provider.BigQueryConfig{}
			bqConfig.Deserialize(offlineStore.Config())
			replacement = fmt.Sprintf("`%s.%s.%s`", bqConfig.ProjectId, bqConfig.DatasetId, replacement)
		} else {
			replacement = sanitize(replacement)
		}
		formattedString += fmt.Sprintf("%s%s", split[0], replacement)
		template = afterSplit[1]
	}
	formattedString += template
	return formattedString, nil
}

func getSourceMapping(template string, replacements map[string]string) ([]provider.SourceMapping, error) {
	sourceMap := []provider.SourceMapping{}
	numEscapes := strings.Count(template, "{{")
	for i := 0; i < numEscapes; i++ {
		split := strings.SplitN(template, "{{", 2)
		afterSplit := strings.SplitN(split[1], "}}", 2)
		key := strings.TrimSpace(afterSplit[0])
		replacement, has := replacements[key]
		if !has {
			return nil, fmt.Errorf("no key set")
		}
		sourceMap = append(sourceMap, provider.SourceMapping{Template: sanitize(replacement), Source: replacement})
		template = afterSplit[1]
	}
	return sourceMap, nil
}

type Coordinator struct {
	Metadata   *metadata.Client
	Logger     *zap.SugaredLogger
	EtcdClient *clientv3.Client
	KVClient   *clientv3.KV
	Spawner    JobSpawner
	Timeout    int
}

type ETCDConfig struct {
	Endpoints []string
	Username  string
	Password  string
}

func (c *ETCDConfig) Serialize() (Config, error) {
	config, err := json.Marshal(c)
	if err != nil {
		panic(err)
	}
	return config, nil
}

func (c *ETCDConfig) Deserialize(config Config) error {
	err := json.Unmarshal(config, c)
	if err != nil {
		return fmt.Errorf("deserialize etcd config: %w", err)
	}
	return nil
}

func (c *Coordinator) AwaitPendingSource(sourceNameVariant metadata.NameVariant) (*metadata.SourceVariant, error) {
	sourceStatus := metadata.PENDING
	for sourceStatus != metadata.READY {
		source, err := c.Metadata.GetSourceVariant(context.Background(), sourceNameVariant)
		if err != nil {
			return nil, err
		}
		sourceStatus := source.Status()
		if sourceStatus == metadata.FAILED {
			return nil, fmt.Errorf("source registration failed: name: %s, variant: %s", sourceNameVariant.Name, sourceNameVariant.Variant)
		}
		if sourceStatus == metadata.READY {
			return source, nil
		}
		time.Sleep(1 * time.Second)
	}
	return c.Metadata.GetSourceVariant(context.Background(), sourceNameVariant)
}

func (c *Coordinator) AwaitPendingFeature(featureNameVariant metadata.NameVariant) (*metadata.FeatureVariant, error) {
	featureStatus := metadata.PENDING
	for featureStatus != metadata.READY {
		feature, err := c.Metadata.GetFeatureVariant(context.Background(), featureNameVariant)
		if err != nil {
			return nil, err
		}
		featureStatus := feature.Status()
		if featureStatus == metadata.FAILED {
			return nil, fmt.Errorf("feature registration failed: name: %s, variant: %s", featureNameVariant.Name, featureNameVariant.Variant)
		}
		if featureStatus == metadata.READY {
			return feature, nil
		}
		time.Sleep(1 * time.Second)
	}
	return c.Metadata.GetFeatureVariant(context.Background(), featureNameVariant)
}

func (c *Coordinator) AwaitPendingLabel(labelNameVariant metadata.NameVariant) (*metadata.LabelVariant, error) {
	labelStatus := metadata.PENDING
	for labelStatus != metadata.READY {
		label, err := c.Metadata.GetLabelVariant(context.Background(), labelNameVariant)
		if err != nil {
			return nil, err
		}
		labelStatus := label.Status()
		if labelStatus == metadata.FAILED {
			return nil, fmt.Errorf("label registration failed: name: %s, variant: %s", labelNameVariant.Name, labelNameVariant.Variant)
		}
		if labelStatus == metadata.READY {
			return label, nil
		}
		time.Sleep(1 * time.Second)
	}
	return c.Metadata.GetLabelVariant(context.Background(), labelNameVariant)
}

type JobSpawner interface {
	GetJobRunner(jobName string, config runner.Config, etcdEndpoints []string, id metadata.ResourceID) (types.Runner, error)
}

type KubernetesJobSpawner struct{}

type MemoryJobSpawner struct{}

func GetLockKey(jobKey string) string {
	return fmt.Sprintf("LOCK_%s", jobKey)
}

func (k *KubernetesJobSpawner) GetJobRunner(jobName string, config runner.Config, etcdEndpoints []string, id metadata.ResourceID) (types.Runner, error) {
	etcdConfig := &ETCDConfig{Endpoints: etcdEndpoints, Username: os.Getenv("ETCD_USERNAME"), Password: os.Getenv("ETCD_PASSWORD")}
	serializedETCD, err := etcdConfig.Serialize()
	if err != nil {
		return nil, err
	}
	pandas_image := help.GetEnv("PANDAS_RUNNER_IMAGE", "featureformcom/k8s_runner:0.3.0-rc")
	fmt.Println("GETJOBRUNNERID:", id)
	kubeConfig := kubernetes.KubernetesRunnerConfig{
		EnvVars: map[string]string{
			"NAME":             jobName,
			"CONFIG":           string(config),
			"ETCD_CONFIG":      string(serializedETCD),
			"K8S_RUNNER_IMAGE": pandas_image,
		},
		Image:               help.GetEnv("WORKER_IMAGE", "local/worker:stable"),
		NumTasks:            1,
		Resource:            id,
		IsCoordinatorRunner: true,
	}
	jobRunner, err := kubernetes.NewKubernetesRunner(kubeConfig)
	if err != nil {
		return nil, err
	}
	return jobRunner, nil
}

func (k *MemoryJobSpawner) GetJobRunner(jobName string, config runner.Config, etcdEndpoints []string, id metadata.ResourceID) (types.Runner, error) {
	jobRunner, err := runner.Create(jobName, config)
	if err != nil {
		return nil, err
	}
	return jobRunner, nil
}

func NewCoordinator(meta *metadata.Client, logger *zap.SugaredLogger, cli *clientv3.Client, spawner JobSpawner) (*Coordinator, error) {
	logger.Info("Creating new coordinator")
	kvc := clientv3.NewKV(cli)
	return &Coordinator{
		Metadata:   meta,
		Logger:     logger,
		EtcdClient: cli,
		KVClient:   &kvc,
		Spawner:    spawner,
		Timeout:    600,
	}, nil
}

const MAX_ATTEMPTS = 1

func (c *Coordinator) WatchForNewJobs() error {
	c.Logger.Info("Watching for new jobs")
	getResp, err := (*c.KVClient).Get(context.Background(), "JOB_", clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("get existing etcd jobs: %w", err)
	}
	for _, kv := range getResp.Kvs {
		go func(kv *mvccpb.KeyValue) {
			err := c.ExecuteJob(string(kv.Key))
			if err != nil {
				switch err.(type) {
				case JobDoesNotExistError:
					c.Logger.Info(err)
				case ResourceAlreadyFailedError:
					c.Logger.Infow("resource has failed previously. Ignoring....", "key", string(kv.Key))
				case ResourceAlreadyCompleteError:
					c.Logger.Infow("resource has already completed. Ignoring....", "key", string(kv.Key))
				default:
					c.Logger.Errorw("Error executing job: Initial search", "error", err)
				}
			}
		}(kv)
	}
	for {
		rch := c.EtcdClient.Watch(context.Background(), "JOB_", clientv3.WithPrefix())
		for wresp := range rch {
			for _, ev := range wresp.Events {
				if ev.Type == 0 {
					go func(ev *clientv3.Event) {
						err := c.ExecuteJob(string(ev.Kv.Key))
						if err != nil {
							re, ok := err.(*JobDoesNotExistError)
							if ok {
								c.Logger.Infow(re.Error())
							} else {
								c.Logger.Errorw("Error executing job: Polling search", "error", err)
							}
						}
					}(ev)
				}

			}
		}
	}
}

func (c *Coordinator) WatchForUpdateEvents() error {
	c.Logger.Info("Watching for new update events")
	for {
		rch := c.EtcdClient.Watch(context.Background(), "UPDATE_EVENT_", clientv3.WithPrefix())
		for wresp := range rch {
			for _, ev := range wresp.Events {
				if ev.Type == 0 {
					go func(ev *clientv3.Event) {
						err := c.signalResourceUpdate(string(ev.Kv.Key), string(ev.Kv.Value))
						if err != nil {
							c.Logger.Errorw("Error executing update event catch: Polling search", "error", err)
						}
					}(ev)
				}

			}
		}
	}
	return nil
}

func (c *Coordinator) WatchForScheduleChanges() error {
	c.Logger.Info("Watching for new update events")
	getResp, err := (*c.KVClient).Get(context.Background(), "SCHEDULEJOB_", clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("fetch existing etcd schedule jobs: %w", err)
	}
	for _, kv := range getResp.Kvs {
		go func(kv *mvccpb.KeyValue) {
			err := c.changeJobSchedule(string(kv.Key), string(kv.Value))
			if err != nil {
				c.Logger.Errorw("Error executing job schedule change: Initial search", "error", err)
			}
		}(kv)
	}
	for {
		rch := c.EtcdClient.Watch(context.Background(), "SCHEDULEJOB_", clientv3.WithPrefix())
		for wresp := range rch {
			for _, ev := range wresp.Events {
				if ev.Type == 0 {
					go func(ev *clientv3.Event) {
						err := c.changeJobSchedule(string(ev.Kv.Key), string(ev.Kv.Value))
						if err != nil {
							c.Logger.Errorw("Error executing job schedule change: Polling search", "error", err)
						}
					}(ev)
				}

			}
		}
	}
	return nil
}

func (c *Coordinator) mapNameVariantsToTables(sources []metadata.NameVariant) (map[string]string, error) {
	sourceMap := make(map[string]string)
	for _, nameVariant := range sources {
		source, err := c.Metadata.GetSourceVariant(context.Background(), nameVariant)
		if err != nil {
			return nil, err
		}
		if source.Status() != metadata.READY {
			return nil, fmt.Errorf("source in query not ready")
		}
		providerResourceID := provider.ResourceID{Name: source.Name(), Variant: source.Variant()}
		var tableName string
		sourceProvider, err := source.FetchProvider(c.Metadata, context.Background())
		if err != nil {
			return nil, fmt.Errorf("Could not fetch source provider: %v", err)
		}
		if sourceProvider.Type() == "SPARK_OFFLINE" && source.IsSQLTransformation() {
			providerResourceID.Type = provider.Transformation
			tableName, err = provider.GetTransformationTableName(providerResourceID)
			if err != nil {
				return nil, err
			}
		} else {
			providerResourceID.Type = provider.Primary
			tableName, err = provider.GetPrimaryTableName(providerResourceID)
			if err != nil {
				return nil, err
			}
		}
		sourceMap[nameVariant.ClientString()] = tableName
	}
	return sourceMap, nil
}

func sanitize(ident string) string {
	return db.Identifier{ident}.Sanitize()
}

func (c *Coordinator) verifyCompletionOfSources(sources []metadata.NameVariant) error {
	allReady := false
	for !allReady {
		sourceVariants, err := c.Metadata.GetSourceVariants(context.Background(), sources)
		if err != nil {
			return fmt.Errorf("could not get source variant: %w ", err)
		}
		total := len(sourceVariants)
		totalReady := 0
		for _, sourceVariant := range sourceVariants {
			if sourceVariant.Status() == metadata.READY {
				totalReady += 1
			}
			if sourceVariant.Status() == metadata.FAILED {
				return fmt.Errorf("dependent source variant failed")
			}
		}
		allReady = total == totalReady
	}
	return nil
}

func (c *Coordinator) runTransformationJob(transformationConfig provider.TransformationConfig, resID metadata.ResourceID, schedule string, sourceProvider *metadata.Provider) error {
	transformation, err := c.Metadata.GetSourceVariant(context.Background(), metadata.NameVariant{resID.Name, resID.Variant})
	if err != nil {
		return fmt.Errorf("get label variant: %w", err)
	}
	status := transformation.Status()

	if status == metadata.READY {
		return ResourceAlreadyCompleteError{
			resourceID: resID,
		}
	}
	if status == metadata.FAILED {
		return ResourceAlreadyFailedError{
			resourceID: resID,
		}
	}

	createTransformationConfig := runner.CreateTransformationConfig{
		OfflineType:          provider.Type(sourceProvider.Type()),
		OfflineConfig:        sourceProvider.SerializedConfig(),
		TransformationConfig: transformationConfig,
		IsUpdate:             false,
	}
	c.Logger.Debugw("Transformation Serialize Config")
	serialized, err := createTransformationConfig.Serialize()
	if err != nil {
		return fmt.Errorf("serialize transformation config: %w", err)
	}
	c.Logger.Debugw("Transformation Get Job Runner")
	jobRunner, err := c.Spawner.GetJobRunner(runner.CREATE_TRANSFORMATION, serialized, c.EtcdClient.Endpoints(), resID)
	if err != nil {
		return fmt.Errorf("spawn create transformation job runner: %w", err)
	}
	c.Logger.Debugw("Transformation Run Job")
	completionWatcher, err := jobRunner.Run()
	if err != nil {
		return fmt.Errorf("run transformation job runner: %w", err)
	}
	c.Logger.Debugw("Transformation Waiting For Completion")
	if err := completionWatcher.Wait(); err != nil {
		return fmt.Errorf("wait for transformation job runner completion: %w", err)
	}
	c.Logger.Debugw("Transformation Setting Status")
	if err := retryWithDelays("set status to ready", 5, time.Millisecond*10, func() error { return c.Metadata.SetStatus(context.Background(), resID, metadata.READY, "") }); err != nil {
		return fmt.Errorf("set transformation job runner done status: %w", err)
	}
	c.Logger.Debugw("Transformation Complete")
	if schedule != "" {
		scheduleCreateTransformationConfig := runner.CreateTransformationConfig{
			OfflineType:          provider.Type(sourceProvider.Type()),
			OfflineConfig:        sourceProvider.SerializedConfig(),
			TransformationConfig: transformationConfig,
			IsUpdate:             true,
		}
		serializedUpdate, err := scheduleCreateTransformationConfig.Serialize()
		if err != nil {
			return fmt.Errorf("serialize schedule transformation config: %w", err)
		}
		jobRunnerUpdate, err := c.Spawner.GetJobRunner(runner.CREATE_TRANSFORMATION, serializedUpdate, c.EtcdClient.Endpoints(), resID)
		if err != nil {
			return fmt.Errorf("run ransformation schedule job runner: %w", err)
		}
		cronRunner, isCronRunner := jobRunnerUpdate.(kubernetes.CronRunner)
		if !isCronRunner {
			return fmt.Errorf("kubernetes runner does not implement schedule")
		}
		if err := cronRunner.ScheduleJob(kubernetes.CronSchedule(schedule)); err != nil {
			return fmt.Errorf("schedule transformation job in kubernetes: %w", err)
		}
		if err := c.Metadata.SetStatus(context.Background(), resID, metadata.READY, ""); err != nil {
			return fmt.Errorf("set transformation succesful schedule status: %w", err)
		}
	}
	return nil
}

func (c *Coordinator) runSQLTransformationJob(transformSource *metadata.SourceVariant, resID metadata.ResourceID, offlineStore provider.OfflineStore, schedule string, sourceProvider *metadata.Provider) error {
	c.Logger.Info("Running SQL transformation job on resource: ", resID)
	templateString := transformSource.SQLTransformationQuery()
	sources := transformSource.SQLTransformationSources()

	err := c.verifyCompletionOfSources(sources)
	if err != nil {
		return fmt.Errorf("the sources were not completed: %s", err)
	}

	sourceMap, err := c.mapNameVariantsToTables(sources)
	if err != nil {
		return fmt.Errorf("map name: %w sources: %v", err, sources)
	}
	sourceMapping, err := getSourceMapping(templateString, sourceMap)
	if err != nil {
		return fmt.Errorf("getSourceMapping replace: %w source map: %v, template: %s", err, sourceMap, templateString)
	}

	var query string
	query, err = templateReplace(templateString, sourceMap, offlineStore)
	if err != nil {
		return fmt.Errorf("template replace: %w source map: %v, template: %s", err, sourceMap, templateString)
	}

	c.Logger.Debugw("Created transformation query", "query", query)
	providerResourceID := provider.ResourceID{Name: resID.Name, Variant: resID.Variant, Type: provider.Transformation}
	transformationConfig := provider.TransformationConfig{Type: provider.SQLTransformation, TargetTableID: providerResourceID, Query: query, SourceMapping: sourceMapping}

	err = c.runTransformationJob(transformationConfig, resID, schedule, sourceProvider)
	if err != nil {
		return err
	}

	return nil
}

func (c *Coordinator) runDFTransformationJob(transformSource *metadata.SourceVariant, resID metadata.ResourceID, offlineStore provider.OfflineStore, schedule string, sourceProvider *metadata.Provider) error {
	c.Logger.Info("Running DF transformation job on resource: ", resID)
	code := transformSource.DFTransformationQuery()
	sources := transformSource.DFTransformationSources()

	err := c.verifyCompletionOfSources(sources)
	if err != nil {
		return fmt.Errorf("the sources were not completed: %s", err)
	}

	sourceMap, err := c.mapNameVariantsToTables(sources)
	if err != nil {
		return fmt.Errorf("map name: %w sources: %v", err, sources)
	}

	sourceMapping := []provider.SourceMapping{}
	for nameVariantClient, transformationTableName := range sourceMap {
		sourceMapping = append(sourceMapping, provider.SourceMapping{Template: nameVariantClient, Source: transformationTableName})
	}

	c.Logger.Debugw("Created transformation query")
	providerResourceID := provider.ResourceID{Name: resID.Name, Variant: resID.Variant, Type: provider.Transformation}
	transformationConfig := provider.TransformationConfig{Type: provider.DFTransformation, TargetTableID: providerResourceID, Code: code, SourceMapping: sourceMapping}

	err = c.runTransformationJob(transformationConfig, resID, schedule, sourceProvider)
	if err != nil {
		return err
	}

	return nil
}

func (c *Coordinator) runPrimaryTableJob(transformSource *metadata.SourceVariant, resID metadata.ResourceID, offlineStore provider.OfflineStore, schedule string) error {
	c.Logger.Info("Running primary table job on resource: ", resID)
	providerResourceID := provider.ResourceID{Name: resID.Name, Variant: resID.Variant, Type: provider.Primary}
	sourceName := transformSource.PrimaryDataSQLTableName()
	if sourceName == "" {
		return fmt.Errorf("no source name set")
	}
	if _, err := offlineStore.RegisterPrimaryFromSourceTable(providerResourceID, sourceName); err != nil {
		return fmt.Errorf("register primary table from source table in offline store: %w", err)
	}
	if err := c.Metadata.SetStatus(context.Background(), resID, metadata.READY, ""); err != nil {
		return fmt.Errorf("set done status for registering primary table: %w", err)
	}
	return nil
}

func (c *Coordinator) runRegisterSourceJob(resID metadata.ResourceID, schedule string) error {
	c.Logger.Info("Running register source job on resource: ", resID)
	source, err := c.Metadata.GetSourceVariant(context.Background(), metadata.NameVariant{resID.Name, resID.Variant})
	if err != nil {
		return fmt.Errorf("get source variant from metadata: %w", err)
	}
	sourceProvider, err := source.FetchProvider(c.Metadata, context.Background())
	if err != nil {
		return fmt.Errorf("fetch source's dependent provider in metadata: %w", err)
	}
	p, err := provider.Get(provider.Type(sourceProvider.Type()), sourceProvider.SerializedConfig())
	if err != nil {
		return fmt.Errorf("get source's dependent provider in offline store: %w", err)
	}
	sourceStore, err := p.AsOfflineStore()
	if err != nil {
		return fmt.Errorf("convert source provider to offline store interface: %w", err)
	}
	defer func(sourceStore provider.OfflineStore) {
		err := sourceStore.Close()
		if err != nil {
			c.Logger.Errorf("could not close offline store: %v", err)
		}
	}(sourceStore)
	if source.IsSQLTransformation() {
		return c.runSQLTransformationJob(source, resID, sourceStore, schedule, sourceProvider)
	} else if source.IsDFTransformation() {
		return c.runDFTransformationJob(source, resID, sourceStore, schedule, sourceProvider)
	} else if source.IsPrimaryDataSQLTable() {
		return c.runPrimaryTableJob(source, resID, sourceStore, schedule)
	} else {
		return fmt.Errorf("source type not implemented")
	}
}

func (c *Coordinator) runLabelRegisterJob(resID metadata.ResourceID, schedule string) error {
	c.Logger.Info("Running label register job: ", resID)
	label, err := c.Metadata.GetLabelVariant(context.Background(), metadata.NameVariant{resID.Name, resID.Variant})
	if err != nil {
		return fmt.Errorf("get label variant: %w", err)
	}
	status := label.Status()
	if status == metadata.READY {
		return ResourceAlreadyCompleteError{
			resourceID: resID,
		}
	}
	if status == metadata.FAILED {
		return ResourceAlreadyFailedError{
			resourceID: resID,
		}
	}
	if err := c.Metadata.SetStatus(context.Background(), resID, metadata.PENDING, ""); err != nil {
		return fmt.Errorf("set pending status for label variant: %w", err)
	}

	sourceNameVariant := label.Source()
	c.Logger.Infow("feature obj", "name", label.Name(), "source", label.Source(), "location", label.Location(), "location_col", label.LocationColumns())

	source, err := c.AwaitPendingSource(sourceNameVariant)
	if err != nil {
		return fmt.Errorf("source of could not complete job: %w", err)
	}
	sourceProvider, err := source.FetchProvider(c.Metadata, context.Background())
	if err != nil {
		return fmt.Errorf("could not fetch online provider: %w", err)
	}
	p, err := provider.Get(provider.Type(sourceProvider.Type()), sourceProvider.SerializedConfig())
	if err != nil {
		return fmt.Errorf("could not get offline provider config: %w", err)
	}
	sourceStore, err := p.AsOfflineStore()
	if err != nil {
		return fmt.Errorf("convert source provider to offline store interface: %w", err)
	}
	defer func(sourceStore provider.OfflineStore) {
		err := sourceStore.Close()
		if err != nil {
			c.Logger.Errorf("could not close offline store: %v", err)
		}
	}(sourceStore)
	var sourceTableName string
	if source.IsSQLTransformation() || source.IsDFTransformation() {
		sourceResourceID := provider.ResourceID{sourceNameVariant.Name, sourceNameVariant.Variant, provider.Transformation}
		sourceTable, err := sourceStore.GetTransformationTable(sourceResourceID)
		if err != nil {
			return err
		}
		sourceTableName = sourceTable.GetName()
	} else if source.IsPrimaryDataSQLTable() {
		sourceResourceID := provider.ResourceID{sourceNameVariant.Name, sourceNameVariant.Variant, provider.Primary}
		sourceTable, err := sourceStore.GetPrimaryTable(sourceResourceID)
		if err != nil {
			return err
		}
		sourceTableName = sourceTable.GetName()
	}

	labelID := provider.ResourceID{
		Name:    resID.Name,
		Variant: resID.Variant,
		Type:    provider.Label,
	}
	tmpSchema := label.LocationColumns().(metadata.ResourceVariantColumns)
	schema := provider.ResourceSchema{
		Entity:      tmpSchema.Entity,
		Value:       tmpSchema.Value,
		TS:          tmpSchema.TS,
		SourceTable: sourceTableName,
	}
	c.Logger.Debugw("Creating Label Resource Table", "id", labelID, "schema", schema)
	_, err = sourceStore.RegisterResourceFromSourceTable(labelID, schema)
	if err != nil {
		return fmt.Errorf("register from source: %w", err)
	}
	c.Logger.Debugw("Resource Table Created", "id", labelID, "schema", schema)

	if err := c.Metadata.SetStatus(context.Background(), resID, metadata.READY, ""); err != nil {
		return fmt.Errorf("set ready status for label variant: %w", err)
	}
	return nil
}

func (c *Coordinator) runFeatureMaterializeJob(resID metadata.ResourceID, schedule string) error {
	c.Logger.Info("Running feature materialization job on resource: ", resID)
	feature, err := c.Metadata.GetFeatureVariant(context.Background(), metadata.NameVariant{resID.Name, resID.Variant})
	if err != nil {
		return fmt.Errorf("get feature variant from metadata: %w", err)
	}
	status := feature.Status()
	featureType := feature.Type()
	if status == metadata.READY {
		return ResourceAlreadyCompleteError{
			resourceID: resID,
		}
	}
	if status == metadata.FAILED {
		return ResourceAlreadyFailedError{
			resourceID: resID,
		}
	}
	if err := c.Metadata.SetStatus(context.Background(), resID, metadata.PENDING, ""); err != nil {
		return fmt.Errorf("set feature variant status to pending: %w", err)
	}

	sourceNameVariant := feature.Source()
	c.Logger.Infow("feature obj", "name", feature.Name(), "source", feature.Source(), "location", feature.Location(), "location_col", feature.LocationColumns())

	source, err := c.AwaitPendingSource(sourceNameVariant)
	if err != nil {
		return fmt.Errorf("source of could not complete job: %w", err)
	}
	sourceProvider, err := source.FetchProvider(c.Metadata, context.Background())
	if err != nil {
		return fmt.Errorf("could not fetch online provider: %w", err)
	}
	p, err := provider.Get(provider.Type(sourceProvider.Type()), sourceProvider.SerializedConfig())
	if err != nil {
		return err
	}
	sourceStore, err := p.AsOfflineStore()
	if err != nil {
		return err
	}
	defer func(sourceStore provider.OfflineStore) {
		err := sourceStore.Close()
		if err != nil {
			c.Logger.Errorf("could not close offline store: %v", err)
		}
	}(sourceStore)
	featureProvider, err := feature.FetchProvider(c.Metadata, context.Background())
	if err != nil {
		return fmt.Errorf("could not fetch  onlineprovider: %w", err)
	}
	materializedRunnerConfig := runner.MaterializedRunnerConfig{
		OnlineType:    provider.Type(featureProvider.Type()),
		OfflineType:   provider.Type(sourceProvider.Type()),
		OnlineConfig:  featureProvider.SerializedConfig(),
		OfflineConfig: sourceProvider.SerializedConfig(),
		ResourceID:    provider.ResourceID{Name: resID.Name, Variant: resID.Variant, Type: provider.Feature},
		VType:         provider.ValueType(featureType),
		Cloud:         runner.LocalMaterializeRunner,
		IsUpdate:      false,
	}
	serialized, err := materializedRunnerConfig.Serialize()
	if err != nil {
		return fmt.Errorf("could not get online provider config: %w", err)
	}
	var sourceTableName string
	if source.IsSQLTransformation() || source.IsDFTransformation() {
		sourceResourceID := provider.ResourceID{sourceNameVariant.Name, sourceNameVariant.Variant, provider.Transformation}
		sourceTable, err := sourceStore.GetTransformationTable(sourceResourceID)
		if err != nil {
			return err
		}
		sourceTableName = sourceTable.GetName()
	} else if source.IsPrimaryDataSQLTable() {
		sourceResourceID := provider.ResourceID{sourceNameVariant.Name, sourceNameVariant.Variant, provider.Primary}
		sourceTable, err := sourceStore.GetPrimaryTable(sourceResourceID)
		if err != nil {
			return err
		}
		sourceTableName = sourceTable.GetName()
	}

	featID := provider.ResourceID{
		Name:    resID.Name,
		Variant: resID.Variant,
		Type:    provider.Feature,
	}
	tmpSchema := feature.LocationColumns().(metadata.ResourceVariantColumns)
	schema := provider.ResourceSchema{
		Entity:      tmpSchema.Entity,
		Value:       tmpSchema.Value,
		TS:          tmpSchema.TS,
		SourceTable: sourceTableName,
	}
	c.Logger.Debugw("Creating Resource Table", "id", featID, "schema", schema)
	_, err = sourceStore.RegisterResourceFromSourceTable(featID, schema)
	if err != nil {
		return fmt.Errorf("materialize feature register: %w", err)
	}
	c.Logger.Debugw("Resource Table Created", "id", featID, "schema", schema)
	needsOnlineMaterialization := strings.Split(string(featureProvider.Type()), "_")[1] == "ONLINE"
	if needsOnlineMaterialization {
		c.Logger.Info("Starting Materialize")
		jobRunner, err := c.Spawner.GetJobRunner(runner.MATERIALIZE, serialized, c.EtcdClient.Endpoints(), resID)
		if err != nil {
			return fmt.Errorf("could not use store as online store: %w", err)
		}
		completionWatcher, err := jobRunner.Run()
		if err != nil {
			return fmt.Errorf("creating watcher for completion runner: %w", err)
		}
		if err := completionWatcher.Wait(); err != nil {
			return fmt.Errorf("completion watcher running: %w", err)
		}
	}
	if err := c.Metadata.SetStatus(context.Background(), resID, metadata.READY, ""); err != nil {
		return fmt.Errorf("materialize set success: %w", err)
	}
	if schedule != "" && needsOnlineMaterialization {
		scheduleMaterializeRunnerConfig := runner.MaterializedRunnerConfig{
			OnlineType:    provider.Type(featureProvider.Type()),
			OfflineType:   provider.Type(sourceProvider.Type()),
			OnlineConfig:  featureProvider.SerializedConfig(),
			OfflineConfig: sourceProvider.SerializedConfig(),
			ResourceID:    provider.ResourceID{Name: resID.Name, Variant: resID.Variant, Type: provider.Feature},
			VType:         provider.ValueType(featureType),
			Cloud:         runner.LocalMaterializeRunner,
			IsUpdate:      true,
		}
		serializedUpdate, err := scheduleMaterializeRunnerConfig.Serialize()
		if err != nil {
			return fmt.Errorf("serialize materialize runner config: %w", err)
		}
		jobRunnerUpdate, err := c.Spawner.GetJobRunner(runner.MATERIALIZE, serializedUpdate, c.EtcdClient.Endpoints(), resID)
		if err != nil {
			return fmt.Errorf("creating materialize job schedule job runner: %w", err)
		}
		cronRunner, isCronRunner := jobRunnerUpdate.(kubernetes.CronRunner)
		if !isCronRunner {
			return fmt.Errorf("kubernetes runner does not implement schedule")
		}
		if err := cronRunner.ScheduleJob(kubernetes.CronSchedule(schedule)); err != nil {
			return fmt.Errorf("schedule materialize job in kubernetes: %w", err)
		}
		if err := c.Metadata.SetStatus(context.Background(), resID, metadata.READY, ""); err != nil {
			return fmt.Errorf("set succesful update status for materialize job in kubernetes: %w", err)
		}
	}
	return nil
}

func (c *Coordinator) runTrainingSetJob(resID metadata.ResourceID, schedule string) error {
	c.Logger.Info("Running training set job on resource: ", "name", resID.Name, "variant", resID.Variant)
	ts, err := c.Metadata.GetTrainingSetVariant(context.Background(), metadata.NameVariant{resID.Name, resID.Variant})
	if err != nil {
		return fmt.Errorf("fetch training set variant from metadata: %w", err)
	}
	status := ts.Status()
	if status == metadata.READY {
		return ResourceAlreadyCompleteError{
			resourceID: resID,
		}
	}
	if status == metadata.FAILED {
		return ResourceAlreadyFailedError{
			resourceID: resID,
		}
	}
	if err := c.Metadata.SetStatus(context.Background(), resID, metadata.PENDING, ""); err != nil {
		return fmt.Errorf("set training set variant status to pending: %w", err)
	}
	providerEntry, err := ts.FetchProvider(c.Metadata, context.Background())
	if err != nil {
		return fmt.Errorf("fetch training set variant offline provider: %w", err)
	}
	p, err := provider.Get(provider.Type(providerEntry.Type()), providerEntry.SerializedConfig())
	if err != nil {
		return fmt.Errorf("fetch offline store interface of training set provider: %w", err)
	}
	store, err := p.AsOfflineStore()
	if err != nil {
		return fmt.Errorf("convert training set provider to offline store interface: %w", err)
	}
	defer func(store provider.OfflineStore) {
		err := store.Close()
		if err != nil {
			c.Logger.Errorf("could not close offline store: %v", err)
		}
	}(store)
	providerResID := provider.ResourceID{Name: resID.Name, Variant: resID.Variant, Type: provider.TrainingSet}
	if _, err := store.GetTrainingSet(providerResID); err == nil {
		return fmt.Errorf("training set already exists: %w", err)
	}
	features := ts.Features()
	featureList := make([]provider.ResourceID, len(features))
	for i, feature := range features {
		featureList[i] = provider.ResourceID{Name: feature.Name, Variant: feature.Variant, Type: provider.Feature}
		featureResource, err := c.Metadata.GetFeatureVariant(context.Background(), feature)
		if err != nil {
			return fmt.Errorf("failed to get fetch dependent feature: %w", err)
		}
		sourceNameVariant := featureResource.Source()
		_, err = c.AwaitPendingSource(sourceNameVariant)
		if err != nil {
			return fmt.Errorf("source of feature could not complete job: %v", err)
		}
		_, err = c.AwaitPendingFeature(metadata.NameVariant{feature.Name, feature.Variant})
		if err != nil {
			return fmt.Errorf("feature could not complete job: %v", err)
		}
	}
	label, err := ts.FetchLabel(c.Metadata, context.Background())
	if err != nil {
		return fmt.Errorf("fetch training set label: %w", err)
	}
	labelSourceNameVariant := label.Source()
	_, err = c.AwaitPendingSource(labelSourceNameVariant)
	if err != nil {
		return fmt.Errorf("source of label could not complete job: %v", err)
	}
	label, err = c.AwaitPendingLabel(metadata.NameVariant{label.Name(), label.Variant()})
	if err != nil {
		return fmt.Errorf("label could not complete job: %v", err)
	}
	trainingSetDef := provider.TrainingSetDef{
		ID:       providerResID,
		Label:    provider.ResourceID{Name: label.Name(), Variant: label.Variant(), Type: provider.Label},
		Features: featureList,
	}
	tsRunnerConfig := runner.TrainingSetRunnerConfig{
		OfflineType:   provider.Type(providerEntry.Type()),
		OfflineConfig: providerEntry.SerializedConfig(),
		Def:           trainingSetDef,
		IsUpdate:      false,
	}
	serialized, _ := tsRunnerConfig.Serialize()
	jobRunner, err := c.Spawner.GetJobRunner(runner.CREATE_TRAINING_SET, serialized, c.EtcdClient.Endpoints(), resID)
	if err != nil {
		return fmt.Errorf("create training set job runner: %w", err)
	}
	completionWatcher, err := jobRunner.Run()
	if err != nil {
		return fmt.Errorf("start training set job runner: %w", err)
	}
	if err := completionWatcher.Wait(); err != nil {
		return fmt.Errorf("wait for training set job runner completion: %w", err)
	}
	if err := c.Metadata.SetStatus(context.Background(), resID, metadata.READY, ""); err != nil {
		return fmt.Errorf("set training set job runner status: %w", err)
	}
	if schedule != "" {
		scheduleTrainingSetRunnerConfig := runner.TrainingSetRunnerConfig{
			OfflineType:   provider.Type(providerEntry.Type()),
			OfflineConfig: providerEntry.SerializedConfig(),
			Def:           trainingSetDef,
			IsUpdate:      true,
		}
		serializedUpdate, err := scheduleTrainingSetRunnerConfig.Serialize()
		if err != nil {
			return fmt.Errorf("serialize training set schedule runner config: %w", err)
		}
		jobRunnerUpdate, err := c.Spawner.GetJobRunner(runner.CREATE_TRAINING_SET, serializedUpdate, c.EtcdClient.Endpoints(), resID)
		if err != nil {
			return fmt.Errorf("spawn training set job runner: %w", err)
		}
		cronRunner, isCronRunner := jobRunnerUpdate.(kubernetes.CronRunner)
		if !isCronRunner {
			return fmt.Errorf("kubernetes runner does not implement schedule")
		}
		if err := cronRunner.ScheduleJob(kubernetes.CronSchedule(schedule)); err != nil {
			return fmt.Errorf("schedule training set job in kubernetes: %w", err)
		}
		if err := c.Metadata.SetStatus(context.Background(), resID, metadata.READY, ""); err != nil {
			return fmt.Errorf("update training set scheduler job status: %w", err)
		}
	}
	return nil
}

func (c *Coordinator) getJob(mtx *concurrency.Mutex, key string) (*metadata.CoordinatorJob, error) {
	c.Logger.Debugf("Checking existence of job with key %s\n", key)
	txn := (*c.KVClient).Txn(context.Background())
	response, err := txn.If(mtx.IsOwner()).Then(clientv3.OpGet(key)).Commit()
	if err != nil {
		return nil, fmt.Errorf("transaction did not succeed: %v", err)
	}
	isOwner := response.Succeeded //response.Succeeded sets true if transaction "if" statement true
	if !isOwner {
		return nil, fmt.Errorf("was not owner of lock")
	}
	responseData := response.Responses[0]
	responseKVs := responseData.GetResponseRange().GetKvs()
	if len(responseKVs) == 0 {
		return nil, &JobDoesNotExistError{key: key}
	}
	responseValue := responseKVs[0].Value //Only single response for single key
	job := &metadata.CoordinatorJob{}
	if err := job.Deserialize(responseValue); err != nil {
		return nil, fmt.Errorf("could not deserialize coordinator job: %v", err)
	}
	return job, nil
}

func (c *Coordinator) incrementJobAttempts(mtx *concurrency.Mutex, job *metadata.CoordinatorJob, jobKey string) error {
	job.Attempts += 1
	serializedJob, err := job.Serialize()
	if err != nil {
		return fmt.Errorf("could not serialize coordinator job. %v", err)
	}
	txn := (*c.KVClient).Txn(context.Background())
	response, err := txn.If(mtx.IsOwner()).Then(clientv3.OpPut(jobKey, string(serializedJob))).Commit()
	if err != nil {
		return fmt.Errorf("could not set iterated coordinator job. %v", err)
	}
	isOwner := response.Succeeded //response.Succeeded sets true if transaction "if" statement true
	if !isOwner {
		return fmt.Errorf("was not owner of lock")
	}
	return nil
}

func (c *Coordinator) deleteJob(mtx *concurrency.Mutex, key string) error {
	c.Logger.Info("Deleting job with key: ", key)
	txn := (*c.KVClient).Txn(context.Background())
	response, err := txn.If(mtx.IsOwner()).Then(clientv3.OpDelete(key)).Commit()
	if err != nil {
		return fmt.Errorf("delete job transaction failed: %v", err)
	}
	isOwner := response.Succeeded //response.Succeeded sets true if transaction "if" statement true
	if !isOwner {
		return fmt.Errorf("was not owner of lock")
	}
	responseData := response.Responses[0] //OpDelete always returns single response
	numDeleted := responseData.GetResponseDeleteRange().Deleted
	if numDeleted != 1 { //returns 0 if delete key did not exist
		return fmt.Errorf("job Already deleted")
	}
	c.Logger.Info("Succesfully deleted job with key: ", key)
	return nil
}

func (c *Coordinator) hasJob(id metadata.ResourceID) (bool, error) {
	getResp, err := (*c.KVClient).Get(context.Background(), metadata.GetJobKey(id), clientv3.WithPrefix())
	if err != nil {
		return false, fmt.Errorf("fetch jobs from etcd with prefix %s: %w", metadata.GetJobKey(id), err)
	}
	responseLength := len(getResp.Kvs)
	if responseLength > 0 {
		return true, nil
	}
	return false, nil
}

func (c *Coordinator) createJobLock(jobKey string, s *concurrency.Session) (*concurrency.Mutex, error) {
	mtx := concurrency.NewMutex(s, GetLockKey(jobKey))
	if err := mtx.Lock(context.Background()); err != nil {
		return nil, fmt.Errorf("create job lock in etcd with key %s: %w", GetLockKey(jobKey), err)
	}
	return mtx, nil
}

func (c *Coordinator) ExecuteJob(jobKey string) error {
	c.Logger.Info("Executing new job with key ", jobKey)
	s, err := concurrency.NewSession(c.EtcdClient, concurrency.WithTTL(1))
	if err != nil {
		return fmt.Errorf("new session: %w", err)
	}
	defer s.Close()
	mtx, err := c.createJobLock(jobKey, s)
	if err != nil {
		return fmt.Errorf("job lock: %w", err)
	}
	defer func() {
		if err := mtx.Unlock(context.Background()); err != nil {
			c.Logger.Debugw("Error unlocking mutex:", "error", err)
		}
	}()
	job, err := c.getJob(mtx, jobKey)
	if err != nil {
		return err
	}
	c.Logger.Debugf("Job %s is on attempt %d", jobKey, job.Attempts)
	if job.Attempts > MAX_ATTEMPTS {
		if err := c.deleteJob(mtx, jobKey); err != nil {
			c.Logger.Debugw("Error deleting job", "error", err)
			return fmt.Errorf("job delete: %w", err)
		}
		return fmt.Errorf("job failed after %d attempts. Cancelling coordinator flow", MAX_ATTEMPTS)
	}
	if err := c.incrementJobAttempts(mtx, job, jobKey); err != nil {
		return fmt.Errorf("increment attempt: %w", err)
	}
	type jobFunction func(metadata.ResourceID, string) error
	fns := map[metadata.ResourceType]jobFunction{
		metadata.TRAINING_SET_VARIANT: c.runTrainingSetJob,
		metadata.FEATURE_VARIANT:      c.runFeatureMaterializeJob,
		metadata.LABEL_VARIANT:        c.runLabelRegisterJob,
		metadata.SOURCE_VARIANT:       c.runRegisterSourceJob,
	}
	jobFunc, has := fns[job.Resource.Type]
	if !has {
		return fmt.Errorf("not a valid resource type for running jobs")
	}
	if err := jobFunc(job.Resource, job.Schedule); err != nil {
		switch err.(type) {
		case ResourceAlreadyFailedError:
			return err
		default:
			statusErr := c.Metadata.SetStatus(context.Background(), job.Resource, metadata.FAILED, err.Error())
			return fmt.Errorf("%s job failed: %v: %v", job.Resource.Type, err, statusErr)
		}
	}
	c.Logger.Info("Succesfully executed job with key: ", jobKey)
	if err := c.deleteJob(mtx, jobKey); err != nil {
		c.Logger.Debugw("Error deleting job", "error", err)
		return fmt.Errorf("job delete: %w", err)
	}
	return nil
}

type ResourceUpdatedEvent struct {
	ResourceID metadata.ResourceID
	Completed  time.Time
}

func (c *ResourceUpdatedEvent) Serialize() (Config, error) {
	config, err := json.Marshal(c)
	if err != nil {
		panic(err)
	}
	return config, nil
}

func (c *ResourceUpdatedEvent) Deserialize(config Config) error {
	err := json.Unmarshal(config, c)
	if err != nil {
		return fmt.Errorf("deserialize resource update event: %w", err)
	}
	return nil
}

func (c *Coordinator) signalResourceUpdate(key string, value string) error {
	c.Logger.Info("Updating metdata with latest resource update status and time", key)
	s, err := concurrency.NewSession(c.EtcdClient, concurrency.WithTTL(1))
	if err != nil {
		return fmt.Errorf("create new concurrency session for resource update job: %w", err)
	}
	defer s.Close()
	mtx, err := c.createJobLock(key, s)
	if err != nil {
		return fmt.Errorf("create lock on resource update job with key %s: %w", key, err)
	}
	defer func() {
		if err := mtx.Unlock(context.Background()); err != nil {
			c.Logger.Debugw("Error unlocking mutex:", "error", err)
		}
	}()
	resUpdatedEvent := &ResourceUpdatedEvent{}
	if err := resUpdatedEvent.Deserialize(Config(value)); err != nil {
		return fmt.Errorf("deserialize resource update event: %w", err)
	}
	if err := c.Metadata.SetStatus(context.Background(), resUpdatedEvent.ResourceID, metadata.READY, ""); err != nil {
		return fmt.Errorf("set resource update status: %w", err)
	}
	c.Logger.Info("Succesfully set update status for update job with key: ", key)
	if err := c.deleteJob(mtx, key); err != nil {
		return fmt.Errorf("delete resource update job: %w", err)
	}
	return nil
}

func (c *Coordinator) changeJobSchedule(key string, value string) error {
	c.Logger.Info("Updating schedule of currently made cronjob in kubernetes: ", key)
	s, err := concurrency.NewSession(c.EtcdClient, concurrency.WithTTL(1))
	if err != nil {
		return fmt.Errorf("create new concurrency session for resource update job: %w", err)
	}
	defer s.Close()
	mtx, err := c.createJobLock(key, s)
	if err != nil {
		return fmt.Errorf("create lock on resource update job with key %s: %w", key, err)
	}
	defer func() {
		if err := mtx.Unlock(context.Background()); err != nil {
			c.Logger.Debugw("Error unlocking mutex:", "error", err)
		}
	}()
	coordinatorScheduleJob := &metadata.CoordinatorScheduleJob{}
	if err := coordinatorScheduleJob.Deserialize(Config(value)); err != nil {
		return fmt.Errorf("deserialize coordiantor schedule job: %w", err)
	}
	namespace, err := kubernetes.GetCurrentNamespace()
	if err != nil {
		return fmt.Errorf("could not get kubernetes namespace: %v", err)
	}
	jobClient, err := kubernetes.NewKubernetesJobClient(kubernetes.GetCronJobName(coordinatorScheduleJob.Resource), namespace)
	if err != nil {
		return fmt.Errorf("create new kubernetes job client: %w", err)
	}
	cronJob, err := jobClient.GetCronJob()
	if err != nil {
		return fmt.Errorf("fetch cron job from kuberentes with name %s: %w", kubernetes.GetCronJobName(coordinatorScheduleJob.Resource), err)
	}
	cronJob.Spec.Schedule = coordinatorScheduleJob.Schedule
	if _, err := jobClient.UpdateCronJob(cronJob); err != nil {
		return fmt.Errorf("update kubernetes cron job: %w", err)
	}
	if err := c.Metadata.SetStatus(context.Background(), coordinatorScheduleJob.Resource, metadata.READY, ""); err != nil {
		return fmt.Errorf("set schedule job update status in metadata: %w", err)
	}
	c.Logger.Info("Succesfully updated schedule for job in kubernetes with key: ", key)
	if err := c.deleteJob(mtx, key); err != nil {
		return fmt.Errorf("delete update schedule job in etcd: %w", err)
	}
	return nil
}
