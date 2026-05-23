## 2024-05-23 - Handle loading states for Server Actions
**Learning:** React Server Actions (like the login form submission) don't automatically provide a loading state to the user. To improve UX by disabling the button and showing a "Sending..." state to prevent double submissions, the submit button must be extracted into a separate Client Component that uses the `useFormStatus` hook from `react-dom`.
**Action:** Always extract submit buttons into Client Components when using Server Actions in forms, and use `useFormStatus` to handle the `pending` state with visual feedback.
