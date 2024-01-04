package provider

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/featureform/helpers"
	pc "github.com/featureform/provider/provider_config"
	pt "github.com/featureform/provider/provider_type"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
)

func TestOfflineStoreRedshift(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration tests")
	}

	secrets := GetSecrets("/testing/redshift")
	err := godotenv.Load("../.env")
	if err != nil {
		t.Logf("could not open .env file... Checking environment: %s", err)
	}

	endpoint := helpers.GetEnv("REDSHIFT_ENDPOINT", secrets["REDSHIFT_ENDPOINT"])

	port := helpers.GetEnv("REDSHIFT_PORT", secrets["REDSHIFT_PORT"])

	username := helpers.GetEnv("REDSHIFT_USERNAME", secrets["REDSHIFT_USERNAME"])

	password := helpers.GetEnv("REDSHIFT_PASSWORD", secrets["REDSHIFT_PASSWORD"])

	redshiftDatabase := fmt.Sprintf("ff%s", strings.ToLower(uuid.NewString()))

	redshiftConfig := pc.RedshiftConfig{
		Endpoint: endpoint,
		Port:     port,
		Database: redshiftDatabase,
		Username: username,
		Password: password,
	}
	serialRSConfig := redshiftConfig.Serialize()
	if err := createRedshiftDatabase(redshiftConfig); err != nil {
		t.Fatalf("%v", err)
	}

	t.Cleanup(func() {
		err := destroyRedshiftDatabase(redshiftConfig)
		if err != nil {
			t.Logf("failed to cleanup database: %s\n", err)
		}
	})

	store, err := GetOfflineStore(pt.RedshiftOffline, serialRSConfig)
	if err != nil {
		t.Fatalf("could not initialize store: %s\n", err)
	}

	test := OfflineStoreTest{
		t:     t,
		store: store,
	}
	test.Run()
	test.RunSQL()
}

func createRedshiftDatabase(c pc.RedshiftConfig) error {
	url := fmt.Sprintf("sslmode=require user=%v password=%s host=%v port=%v dbname=%v", c.Username, c.Password, c.Endpoint, c.Port, "dev")
	db, err := sql.Open("postgres", url)
	if err != nil {
		return err
	}
	databaseQuery := fmt.Sprintf("CREATE DATABASE %s", sanitize(c.Database))
	if _, err := db.Exec(databaseQuery); err != nil {
		return err
	}
	fmt.Printf("Created Redshift Database %s\n", c.Database)
	return nil
}

func destroyRedshiftDatabase(c pc.RedshiftConfig) error {
	url := fmt.Sprintf("sslmode=require user=%v password=%s host=%v port=%v dbname=%v", c.Username, c.Password, c.Endpoint, c.Port, "dev")
	db, err := sql.Open("postgres", url)
	if err != nil {
		fmt.Errorf(err.Error())
		return err
	}
	disconnectQuery := fmt.Sprintf("SELECT pg_terminate_backend(pg_stat_activity.procpid) FROM pg_stat_activity WHERE datid=(SELECT oid from pg_database where datname = '%s');", c.Database)
	if _, err := db.Exec(disconnectQuery); err != nil {
		fmt.Errorf(err.Error())
		return err
	}
	var deleteErr error
	retries := 5
	databaseQuery := fmt.Sprintf("DROP DATABASE %s", sanitize(c.Database))
	for {
		if _, err := db.Exec(databaseQuery); err != nil {
			deleteErr = err
			time.Sleep(time.Second)
			retries--
			if retries == 0 {
				fmt.Errorf(err.Error())
				return deleteErr
			}
		} else {
			continue
		}
	}
}
