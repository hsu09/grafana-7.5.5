import React, { Component } from 'react';

import { Button, Select, Table } from '@grafana/ui';
import {
  AppEvents,
  ArrayVector,
  DataFrame,
  FieldMatcherID,
  getFieldDisplayName,
  getFrameDisplayName,
  PanelProps,
  SelectableValue,
} from '@grafana/data';
import { getBackendSrv, getDataSourceSrv } from '@grafana/runtime';
import { Options } from './types';
import { css } from 'emotion';
import { config } from 'app/core/config';
import { FilterItem, TableFieldOptions, TableSortByFieldState } from '@grafana/ui/src/components/Table/types';
import { dispatch } from '../../../store/store';
import { applyFilterFromTable } from '../../../features/variables/adhoc/actions';
import { getDashboardSrv } from '../../../features/dashboard/services/DashboardSrv';
import { exportDataFrameToExcel } from './excel';
import appEvents from 'app/core/app_events';

interface Props extends PanelProps<Options> {}

interface State {
  exporting: boolean;
  exportStatus: string;
}

interface TableExportDatasource {
  getTableExportRequest?: (
    request: unknown
  ) => {
    from: string;
    to: string;
    queries: unknown[];
  };
}

const TABLE_PREVIEW_ROW_LIMIT = 20000;
const SERVER_EXPORT_ROW_THRESHOLD = 100000;

export class TablePanel extends Component<Props, State> {
  state: State = {
    exporting: false,
    exportStatus: '',
  };

  private previewFrames = new WeakMap<DataFrame, DataFrame>();

  onColumnResize = (fieldDisplayName: string, width: number) => {
    const { fieldConfig } = this.props;
    const { overrides } = fieldConfig;

    const matcherId = FieldMatcherID.byName;
    const propId = 'custom.width';

    // look for existing override
    const override = overrides.find((o) => o.matcher.id === matcherId && o.matcher.options === fieldDisplayName);

    if (override) {
      // look for existing property
      const property = override.properties.find((prop) => prop.id === propId);
      if (property) {
        property.value = width;
      } else {
        override.properties.push({ id: propId, value: width });
      }
    } else {
      overrides.push({
        matcher: { id: matcherId, options: fieldDisplayName },
        properties: [{ id: propId, value: width }],
      });
    }

    this.props.onFieldConfigChange({
      ...fieldConfig,
      overrides,
    });
  };

  onSortByChange = (sortBy: TableSortByFieldState[]) => {
    this.props.onOptionsChange({
      ...this.props.options,
      sortBy,
    });
  };

  onChangeTableSelection = (val: SelectableValue<number>) => {
    this.props.onOptionsChange({
      ...this.props.options,
      frameIndex: val.value || 0,
    });

    // Force a redraw -- but no need to re-query
    this.forceUpdate();
  };

  onCellFilterAdded = (filter: FilterItem) => {
    const { key, value, operator } = filter;
    const panelModel = getDashboardSrv().getCurrent().getPanelById(this.props.id);
    const datasource = panelModel?.datasource;

    if (!datasource) {
      return;
    }

    dispatch(applyFilterFromTable({ datasource, key, operator, value }));
  };

  onExportExcel = async (frame: DataFrame) => {
    if (this.state.exporting) {
      return;
    }

    this.setState({
      exporting: true,
      exportStatus: `Preparing ${frame.length.toLocaleString()} rows...`,
    });

    try {
      const exportedOnServer = await this.exportLargeSqlResult(frame);
      if (!exportedOnServer) {
        await exportDataFrameToExcel(frame, this.props.title, this.props.options.sortBy || [], (completed, total) => {
          const percent = total > 0 ? Math.round((completed / total) * 100) : 100;
          this.setState({ exportStatus: `Exporting ${percent}%...` });
        });
      }
      appEvents.emit(AppEvents.alertSuccess, ['Excel export completed']);
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      appEvents.emit(AppEvents.alertError, ['Excel export failed', message]);
    } finally {
      this.setState({ exporting: false, exportStatus: '' });
    }
  };

  exportLargeSqlResult = async (frame: DataFrame): Promise<boolean> => {
    if (frame.length < SERVER_EXPORT_ROW_THRESHOLD || !this.props.data.request) {
      return false;
    }

    const panelModel = getDashboardSrv().getCurrent().getPanelById(this.props.id);
    const datasource = ((await getDataSourceSrv().get(panelModel?.datasource)) as unknown) as TableExportDatasource;
    if (typeof datasource.getTableExportRequest !== 'function') {
      return false;
    }

    const queryRequest = datasource.getTableExportRequest(this.props.data.request);
    if (!queryRequest.queries.length) {
      return false;
    }

    this.setState({ exportStatus: `Server export: ${frame.length.toLocaleString()}+ rows...` });
    const response = await getBackendSrv()
      .fetch<Blob>({
        url: '/api/tsdb/query/xlsx',
        method: 'POST',
        data: {
          ...queryRequest,
          title: this.props.title,
          columns: frame.fields.map((field) => {
            const options = (field.config.custom || {}) as TableFieldOptions;
            return {
              name: field.name,
              displayName: getFieldDisplayName(field, frame),
              hidden: Boolean(options.hidden),
            };
          }),
        },
        responseType: 'blob',
        showSuccessAlert: false,
        hideFromInspector: true,
      })
      .toPromise();

    if (!response?.data) {
      throw new Error('The server returned an empty Excel file');
    }

    downloadBlob(response.data, getExportFilename(response.headers, this.props.title));
    return true;
  };

  getPreviewFrame(frame: DataFrame): DataFrame {
    if (frame.length <= TABLE_PREVIEW_ROW_LIMIT) {
      return frame;
    }

    const cached = this.previewFrames.get(frame);
    if (cached) {
      return cached;
    }

    const preview: DataFrame = {
      ...frame,
      length: TABLE_PREVIEW_ROW_LIMIT,
      fields: frame.fields.map((field) => {
        const values = new Array(TABLE_PREVIEW_ROW_LIMIT);
        for (let index = 0; index < TABLE_PREVIEW_ROW_LIMIT; index++) {
          values[index] = field.values.get(index);
        }
        return {
          ...field,
          values: new ArrayVector(values),
        };
      }),
    };
    this.previewFrames.set(frame, preview);
    return preview;
  }

  renderTable(frame: DataFrame, width: number, height: number) {
    const { options } = this.props;

    return (
      <Table
        height={height}
        width={width}
        data={frame}
        noHeader={!options.showHeader}
        resizable={true}
        initialSortBy={options.sortBy}
        onSortByChange={this.onSortByChange}
        onColumnResize={this.onColumnResize}
        onCellFilterAdded={this.onCellFilterAdded}
      />
    );
  }

  getCurrentFrameIndex() {
    const { data, options } = this.props;
    const count = data.series?.length;
    return options.frameIndex > 0 && options.frameIndex < count ? options.frameIndex : 0;
  }

  renderToolbar(frame: DataFrame, selector?: React.ReactNode) {
    const previewed = frame.length > TABLE_PREVIEW_ROW_LIMIT;
    return (
      <div className={tableStyles.toolbar}>
        {selector}
        {previewed && (
          <div className={tableStyles.previewInfo}>
            Showing {TABLE_PREVIEW_ROW_LIMIT.toLocaleString()} of {frame.length.toLocaleString()} rows; export includes
            all
          </div>
        )}
        <Button
          icon="download-alt"
          size="sm"
          variant="secondary"
          disabled={this.state.exporting}
          onClick={() => this.onExportExcel(frame)}
        >
          {this.state.exportStatus || 'Export Excel'}
        </Button>
      </div>
    );
  }

  render() {
    const { data, height, width } = this.props;

    const count = data.series?.length;
    const hasFields = data.series[0]?.fields.length;

    if (!count || !hasFields) {
      return <div>No data</div>;
    }

    const inputHeight = config.theme.spacing.formInputHeight;
    const padding = 8 * 2;
    const toolbarHeight = inputHeight + padding;

    if (count > 1) {
      const currentIndex = this.getCurrentFrameIndex();
      const currentFrame = data.series[currentIndex];
      const names = data.series.map((frame, index) => {
        return {
          label: getFrameDisplayName(frame),
          value: index,
        };
      });

      return (
        <div className={tableStyles.wrapper}>
          {this.renderToolbar(
            currentFrame,
            <div className={tableStyles.selectWrapper}>
              <Select options={names} value={names[currentIndex]} onChange={this.onChangeTableSelection} />
            </div>
          )}
          {this.renderTable(this.getPreviewFrame(currentFrame), width, height - toolbarHeight)}
        </div>
      );
    }

    const currentFrame = data.series[0];
    return (
      <div className={tableStyles.wrapper}>
        {this.renderToolbar(currentFrame)}
        {this.renderTable(this.getPreviewFrame(currentFrame), width, height - toolbarHeight)}
      </div>
    );
  }
}

const tableStyles = {
  wrapper: css`
    display: flex;
    flex-direction: column;
    height: 100%;
  `,
  toolbar: css`
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: 8px;
    padding: 8px;
    min-height: 48px;
  `,
  previewInfo: css`
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: ${config.theme.colors.textWeak};
    text-align: right;
  `,
  selectWrapper: css`
    flex: 1;
    min-width: 0;
  `,
};

function getExportFilename(headers: Headers, panelTitle: string): string {
  const disposition = headers.get('content-disposition') || '';
  const encoded = disposition.match(/filename\*=UTF-8''([^;]+)/i);
  if (encoded) {
    try {
      return decodeURIComponent(encoded[1]);
    } catch {}
  }

  const quoted = disposition.match(/filename="([^"]+)"/i);
  if (quoted) {
    return quoted[1];
  }

  const safeTitle = panelTitle.replace(/[\\/:*?"<>|]+/g, '_').trim() || 'grafana-table';
  return `${safeTitle}-${new Date().toISOString().slice(0, 19).replace('T', '_').replace(/:/g, '-')}.xlsx`;
}

function downloadBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a');
  link.href = url;
  link.download = filename;
  link.style.display = 'none';
  document.body.appendChild(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}
