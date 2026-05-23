## 2024-05-19 - Parallelizing Independent DB Queries in Next.js Server Components
**Learning:** Sequential `await` calls for independent database queries in React Server Components unnecessarily increase server response time (TTFB).
**Action:** Always identify independent data fetching requirements in Server Components and execute them concurrently using `Promise.all` to reduce total latency.
