## 2024-11-20 - Missing loading state on Server Action forms
**Learning:** React Server Actions used in Next.js forms do not automatically provide a loading state on the button element (like `isPending` in `useActionState`), making async operations feel unresponsive.
**Action:** Extract form submit buttons into separate Client Components utilizing the `useFormStatus` hook to manage `pending` states, update button text (e.g. "Sending..."), and apply disabled styles, ensuring visual feedback and preventing double submissions.
