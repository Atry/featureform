import CloseIcon from '@mui/icons-material/Close';
import { Box, IconButton, TextField, Typography } from '@mui/material';
import { DataGrid } from '@mui/x-data-grid';
import React, { useState } from 'react';

export default function VariantView({
  variantList = [],
  handleClose = () => null,
  handleSelect = () => null,
  handleSearch = () => null,
}) {
  const columns = [
    {
      field: 'id',
      headerName: 'id',
      flex: 1,
      width: 100,
      editable: false,
      sortable: false,
      filterable: false,
      hide: true,
    },
    {
      field: 'name',
      headerName: 'Name',
      flex: 1,
      width: 100,
      editable: false,
      sortable: false,
      filterable: false,
      hide: true,
    },
    {
      field: 'variant',
      headerName: 'Variant',
      flex: 1,
      width: 125,
      editable: false,
      sortable: false,
      filterable: false,
    },
    {
      field: 'owner',
      headerName: 'Owner',
      flex: 1,
      width: 125,
      editable: false,
      sortable: false,
      filterable: false,
    },
  ];

  const ENTER_KEY = 'Enter';
  const [searchQuery, setSearchQuery] = useState('');

  const handleRowSelect = (selectedRow) => {
    if (selectedRow?.row) {
      handleSelect?.(selectedRow.row.variant);
    }
  };

  return (
    <Box data-testid='variantViewId'>
      <Box sx={{ margin: '1em', width: 650 }}>
        <Box sx={{ margin: 1 }}>
          <Typography align='center'>Variants</Typography>
          <IconButton
            size='large'
            onClick={() => handleClose?.()}
            sx={{ position: 'absolute', right: '10px', top: '5px' }}
          >
            <CloseIcon />
          </IconButton>
        </Box>
        <TextField
          variant='outlined'
          margin='normal'
          autoFocus
          fullWidth
          label='Search Variants'
          name='searchVariants'
          inputProps={{
            'aria-label': 'search variant input',
            'data-testid': 'searchVariantInputId',
          }}
          onChange={(event) => {
            const rawText = event.target.value;
            if (rawText === '') {
              // user is deleting the text field. allow this and clear out state
              setSearchQuery(rawText);
              handleSearch('');
              return;
            }
            const searchText = event.target.value ?? '';
            if (searchText.trim()) {
              setSearchQuery(searchText);
            }
          }}
          value={searchQuery}
          onKeyDown={(event) => {
            if (event.key === ENTER_KEY && searchQuery) {
              handleSearch(searchQuery);
            }
          }}
        />
      </Box>
      <Box sx={{ margin: 1 }}>
        <DataGrid
          disableVirtualization
          density='compact'
          autoHeight
          sx={{
            '& .MuiDataGrid-cell:focus': {
              outline: 'none',
            },
            '& .MuiDataGrid-columnHeader:focus': {
              outline: 'none',
            },
            '& .MuiDataGrid-columnHeaderTitle': {
              fontWeight: 'bold',
            },
            marginBottom: '1.5em',
            cursor: 'pointer',
          }}
          aria-label='Other Runs'
          rows={variantList}
          onRowClick={handleRowSelect}
          rowsPerPageOptions={[25]}
          columns={columns}
          initialState={{
            pagination: { paginationModel: { page: 0, pageSize: 25 } },
          }}
          hideFooter
          pageSize={5}
          disableColumnMenu
          disableColumnSelector
        />
      </Box>
    </Box>
  );
}
