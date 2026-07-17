import React, { Component } from 'react';

import { Button, Select, Table } from '@grafana/ui';
import { DataFrame, FieldMatcherID, getFrameDisplayName, PanelProps, SelectableValue } from '@grafana/data';
import { Options } from './types';
import { css } from 'emotion';
import { config } from 'app/core/config';
import { FilterItem, TableSortByFieldState } from '@grafana/ui/src/components/Table/types';
import { dispatch } from '../../../store/store';
import { applyFilterFromTable } from '../../../features/variables/adhoc/actions';
import { getDashboardSrv } from '../../../features/dashboard/services/DashboardSrv';
import { exportDataFrameToExcel } from './excel';

interface Props extends PanelProps<Options> {}

export class TablePanel extends Component<Props> {
  constructor(props: Props) {
    super(props);
  }

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

  onExportExcel = (frame: DataFrame) => {
    exportDataFrameToExcel(frame, this.props.title, this.props.options.sortBy || []);
  };

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
          {this.renderTable(currentFrame, width, height - toolbarHeight)}
          <div className={tableStyles.toolbar}>
            <div className={tableStyles.selectWrapper}>
              <Select options={names} value={names[currentIndex]} onChange={this.onChangeTableSelection} />
            </div>
            <Button
              icon="download-alt"
              size="sm"
              variant="secondary"
              onClick={() => this.onExportExcel(currentFrame)}
            >
              Export Excel
            </Button>
          </div>
        </div>
      );
    }

    const currentFrame = data.series[0];
    return (
      <div className={tableStyles.wrapper}>
        {this.renderTable(currentFrame, width, height - toolbarHeight)}
        <div className={tableStyles.toolbar}>
          <Button
            icon="download-alt"
            size="sm"
            variant="secondary"
            onClick={() => this.onExportExcel(currentFrame)}
          >
            Export Excel
          </Button>
        </div>
      </div>
    );
  }
}

const tableStyles = {
  wrapper: css`
    display: flex;
    flex-direction: column;
    justify-content: space-between;
    height: 100%;
  `,
  toolbar: css`
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: 8px;
    padding: 8px;
  `,
  selectWrapper: css`
    flex: 1;
    min-width: 0;
  `,
};
