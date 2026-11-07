// Review dates for external dependents: the forms take a date
// (midnight UTC) or a full RFC 3339 time.

const midnight = 'T00:00:00Z';

/** Default review horizon for a new declaration or a review: 90 days. */
const REVIEW_DAYS = 90;

/** A date alone means midnight UTC; anything else goes to the server as typed. */
export function toReviewAt(v: string): string {
  const s = v.trim();
  return /^\d{4}-\d{2}-\d{2}$/.test(s) ? s + midnight : s;
}

export function fromReviewAt(v: string): string {
  return v.endsWith(midnight) ? v.slice(0, -midnight.length) : v;
}

/** Today + 90 days, as YYYY-MM-DD (UTC). */
export function defaultReviewDate(): string {
  return new Date(Date.now() + REVIEW_DAYS * 86_400_000).toISOString().slice(0, 10);
}
