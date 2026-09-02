import React, { Component } from 'react';

import { Button, ClipboardButton, Modal, Select, Table, TextArea } from '@grafana/ui';
import {
  AppEvents,
  DataFrame,
  FieldMatcherID,
  getFieldDisplayName,
  getFrameDisplayName,
  LoadingState,
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
import { buildPlainTextTable, TABLE_TEXT_ROW_LIMIT } from './plainText';
import appEvents from 'app/core/app_events';

interface Props extends PanelProps<Options> {}

interface State {
  exporting: boolean;
  exportStatus: string;
  counting: boolean;
  totalRows?: number;
  showFullList: boolean;
}

interface TableCountResponse {
  totalRows: number;
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

const TABLE_PREVIEW_ROW_LIMIT = 500000;

export class TablePanel extends Component<Props, State> {
  state: State = {
    exporting: false,
    exportStatus: '',
    counting: false,
    showFullList: false,
  };

  private countRequestId = 0;

  componentDidMount() {
    this.updateTotalRows();
  }

  componentDidUpdate(prevProps: Props) {
    if (prevProps.data.request !== this.props.data.request) {
      this.updateTotalRows();
    }
  }

  componentWillUnmount() {
    this.countRequestId++;
  }

  updateTotalRows = async () => {
    const requestId = ++this.countRequestId;
    if (!this.props.data.request) {
      this.setState({ counting: false, totalRows: undefined });
      return;
    }

    // Count independently and in parallel with the capped preview query. This makes
    // totals available without waiting for up to 500,000 detail rows to download.
    this.setState({ counting: true, totalRows: undefined });
    try {
      const panelModel = getDashboardSrv().getCurrent().getPanelById(this.props.id);
      const datasource = ((await getDataSourceSrv().get(panelModel?.datasource)) as unknown) as TableExportDatasource;
      if (typeof datasource.getTableExportRequest !== 'function') {
        return;
      }

      const queryRequest = datasource.getTableExportRequest(this.props.data.request);
      if (!queryRequest.queries.length) {
        return;
      }

      const response = await getBackendSrv()
        .fetch<TableCountResponse>({
          url: '/api/tsdb/query/count',
          method: 'POST',
          data: queryRequest,
          showSuccessAlert: false,
          hideFromInspector: true,
        })
        .toPromise();

      const totalRows = Number(response?.data?.totalRows);
      if (requestId === this.countRequestId && Number.isFinite(totalRows) && totalRows >= 0) {
        this.setState({ totalRows });
      }
    } catch {
      // Fall back to the preview frame count when an exact SQL count is unavailable.
    } finally {
      if (requestId === this.countRequestId) {
        this.setState({ counting: false });
      }
    }
  };

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
      exportStatus: 'Preparing...',
    });

    try {
      const exportedOnServer = await this.exportSqlResult(frame);
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

  exportSqlResult = async (frame: DataFrame): Promise<boolean> => {
    if (!this.props.data.request) {
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

    this.setState({ exportStatus: 'Exporting...' });
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

  renderTable(frame: DataFrame, width: number, height: number) {
    const { options } = this.props;

    return (
      <Table
        height={height}
        width={width}
        data={frame}
        maxRows={TABLE_PREVIEW_ROW_LIMIT}
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

  renderToolbar(frame?: DataFrame, selector?: React.ReactNode) {
    const previewIsCapped = Boolean(frame && frame.length >= TABLE_PREVIEW_ROW_LIMIT);
    const rowCount = this.state.totalRows ?? frame?.length;
    const rowCountSuffix = previewIsCapped && this.state.totalRows === undefined ? '+' : '';
    return (
      <div className={tableStyles.toolbar}>
        {selector}
        <div className={tableStyles.previewInfo}>
          {rowCount === undefined ? 'Counting rows...' : `${rowCount.toLocaleString()}${rowCountSuffix} rows`}
        </div>
        <Button
          icon="eye"
          size="sm"
          variant="secondary"
          disabled={!frame}
          onClick={() => this.setState({ showFullList: true })}
        >
          Full list
        </Button>
        <Button
          icon="download-alt"
          size="sm"
          variant="secondary"
          disabled={this.state.exporting || !frame}
          onClick={() => frame && this.onExportExcel(frame)}
        >
          {this.state.exportStatus || 'Export'}
        </Button>
      </div>
    );
  }

  renderFullList(frame: DataFrame) {
    if (!this.state.showFullList) {
      return null;
    }

    const plainText = buildPlainTextTable(frame, TABLE_TEXT_ROW_LIMIT);
    const description = plainText.truncated
      ? `Showing the first ${plainText.rowCount.toLocaleString()} of ${frame.length.toLocaleString()} rows. Use Export for all rows.`
      : `${plainText.rowCount.toLocaleString()} rows. Values are complete and not visually truncated.`;

    return (
      <Modal
        title={`${this.props.title} - Full list`}
        icon="table"
        isOpen={true}
        onDismiss={() => this.setState({ showFullList: false })}
        className={tableStyles.fullListModal}
        contentClassName={tableStyles.fullListContent}
      >
        <div className={tableStyles.fullListDescription}>{description}</div>
        <TextArea
          className={tableStyles.fullListText}
          value={plainText.text}
          readOnly={true}
          wrap="soft"
          aria-label="Full table text"
        />
        <div className={tableStyles.fullListActions}>
          <ClipboardButton
            icon="copy"
            size="sm"
            variant="primary"
            getText={() => plainText.text}
            onClipboardCopy={() => appEvents.emit(AppEvents.alertSuccess, ['Full table copied'])}
            onClipboardError={() => appEvents.emit(AppEvents.alertError, ['Unable to copy table'])}
          >
            Copy
          </ClipboardButton>
          <Button size="sm" variant="secondary" onClick={() => this.setState({ showFullList: false })}>
            Close
          </Button>
        </div>
      </Modal>
    );
  }

  render() {
    const { data, height, width } = this.props;

    const count = data.series?.length;
    const hasFields = data.series[0]?.fields.length;

    if (!count || !hasFields) {
      return (
        <div className={tableStyles.wrapper}>
          {this.renderToolbar()}
          <div className={tableStyles.emptyState}>
            {data.state === LoadingState.Loading ? 'Loading preview...' : 'No data'}
          </div>
        </div>
      );
    }

    const toolbarHeight = Math.max(0, config.theme.spacing.formInputHeight - config.theme.panelHeaderHeight);

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
        <>
          <div className={tableStyles.wrapper}>
            {this.renderToolbar(
              currentFrame,
              <div className={tableStyles.selectWrapper}>
                <Select options={names} value={names[currentIndex]} onChange={this.onChangeTableSelection} />
              </div>
            )}
            {this.renderTable(currentFrame, width, height - toolbarHeight)}
          </div>
          {this.renderFullList(currentFrame)}
        </>
      );
    }

    const currentFrame = data.series[0];
    return (
      <>
        <div className={tableStyles.wrapper}>
          {this.renderToolbar(currentFrame)}
          {this.renderTable(currentFrame, width, height - toolbarHeight)}
        </div>
        {this.renderFullList(currentFrame)}
      </>
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
    padding: 0 40px 0 8px;
    min-height: ${config.theme.spacing.formInputHeight}px;
    margin-top: -${config.theme.panelHeaderHeight}px;
    position: relative;
    z-index: 12;
    pointer-events: none;

    button {
      pointer-events: auto;
    }
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
    pointer-events: auto;
  `,
  fullListModal: css`
    width: 92vw;
    max-width: 1800px;
    top: 4vh;
  `,
  fullListContent: css`
    display: flex;
    flex-direction: column;
    max-height: calc(92vh - 42px);
  `,
  fullListDescription: css`
    color: ${config.theme.colors.textWeak};
    margin-bottom: 8px;
  `,
  fullListText: css`
    flex: 1;
    min-height: 68vh;
    resize: none;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    font-family: ${config.theme.typography.fontFamily.monospace};
    line-height: 1.45;
  `,
  fullListActions: css`
    display: flex;
    justify-content: flex-end;
    gap: 8px;
    margin-top: 12px;
  `,
  emptyState: css`
    display: flex;
    align-items: center;
    justify-content: center;
    flex: 1;
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
