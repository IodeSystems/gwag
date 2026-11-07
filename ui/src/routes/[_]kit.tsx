import { createFileRoute } from '@tanstack/react-router';
import { Showcase, type View } from '@/ui/showcase/Showcase';

const views: View[] = ['gallery', 'current', 'dark', 'light', 'both'];

// UI kit gallery + showcase. `/_kit` opens the preset gallery;
// `/_kit?view=current|dark|light|both` opens the full component views.
// Filename uses [_] so the router treats `_kit` as a literal path
// segment instead of a pathless layout.
export const Route = createFileRoute('/_kit')({
  validateSearch: (search: Record<string, unknown>): { view: View } => ({
    view: views.find((v) => v === search.view) ?? 'gallery',
  }),
  component: KitPage,
});

function KitPage() {
  const { view } = Route.useSearch();
  const navigate = Route.useNavigate();
  return <Showcase view={view} onViewChange={(v) => void navigate({ search: { view: v } })} />;
}
