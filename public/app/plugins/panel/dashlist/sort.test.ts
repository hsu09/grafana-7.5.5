import { compareDashboards } from './sort';

describe('compareDashboards', () => {
  it('places dashboards tagged order-last after regular dashboards', () => {
    const dashboards = [
      { title: 'Dell Unity380', tags: ['order-last'] },
      { title: 'PVE', tags: [] },
      { title: 'Farm resources', tags: [] },
    ];

    expect(dashboards.sort(compareDashboards).map((dashboard) => dashboard.title)).toEqual([
      'Farm resources',
      'PVE',
      'Dell Unity380',
    ]);
  });

  it('matches the tag case-insensitively and alphabetizes dashboards inside the last group', () => {
    const dashboards = [
      { title: 'Zeta', tags: ['ORDER-LAST'] },
      { title: 'Alpha', tags: ['order-last'] },
      { title: 'Normal', tags: ['order-last-extra'] },
    ];

    expect(dashboards.sort(compareDashboards).map((dashboard) => dashboard.title)).toEqual(['Normal', 'Alpha', 'Zeta']);
  });
});
