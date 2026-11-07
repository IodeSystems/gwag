import { createFileRoute } from '@tanstack/react-router';
import { Showcase } from '@/ui/showcase/Showcase';

// UI kit showcase. Every kit component in every state, both themes.
// Filename uses [_] so the router treats `_kit` as a literal path
// segment instead of a pathless layout.
export const Route = createFileRoute('/_kit')({
  component: Showcase,
});
