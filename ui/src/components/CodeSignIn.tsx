import { useEffect } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useToast } from '@/ui';
import { redeemSignInCode, takeSignInCode } from '@/api/sso';

/**
 * CodeSignIn redeems the sign-in code gwag login --web put in the URL
 * (captured by captureSignInCode before the router ran), then refetches
 * every query so the role and session state update. Renders nothing.
 */
export default function CodeSignIn() {
  const queryClient = useQueryClient();
  const toast = useToast();
  useEffect(() => {
    // takeSignInCode returns the code once, so StrictMode's second
    // effect run does not spend a used code.
    const code = takeSignInCode();
    if (!code) return;
    redeemSignInCode(code)
      .then((r) => {
        toast.success(`Signed in as ${r.subject} (${r.role}).`);
      })
      .catch((e: unknown) => {
        toast.error(`Sign-in link not accepted: ${e instanceof Error ? e.message : String(e)}`);
      })
      .finally(() => {
        void queryClient.invalidateQueries();
      });
  }, [queryClient, toast]);
  return null;
}
