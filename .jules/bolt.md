## 2025-02-12 - Parallelized Data Fetching in Next.js Server Components
**Learning:** Sequential `await` statements in Next.js Server Components block the render and increase TTFB, even when queries are independent (e.g. fetching API keys and usage stats).
**Action:** Always identify independent data fetching requirements in Server Components and parallelize them using `Promise.all` to minimize latency.
