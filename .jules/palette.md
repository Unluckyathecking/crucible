## 2026-05-16 - Add login button loading state
**Learning:** In the Next.js dashboard, React Server Actions used in forms do not automatically provide a loading state. Extract submit buttons into separate Client Components utilizing the `useFormStatus` hook to manage pending states, provide visual feedback, and prevent double submissions.
**Action:** When implementing forms using Server Actions, always extract the submit button into a separate client component and use `useFormStatus` to handle the pending state.
