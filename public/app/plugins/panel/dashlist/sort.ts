import { DashboardSearchHit } from 'app/features/search/types';

export const ORDER_LAST_TAG = 'order-last';

type SortableDashboard = Pick<DashboardSearchHit, 'tags' | 'title'>;

function hasOrderLastTag(dashboard: SortableDashboard): boolean {
  return (dashboard.tags || []).some((tag) => tag.toLowerCase() === ORDER_LAST_TAG);
}

export function compareDashboards(a: SortableDashboard, b: SortableDashboard): number {
  const aIsLast = hasOrderLastTag(a);
  const bIsLast = hasOrderLastTag(b);

  if (aIsLast !== bIsLast) {
    return aIsLast ? 1 : -1;
  }

  return a.title.localeCompare(b.title);
}
