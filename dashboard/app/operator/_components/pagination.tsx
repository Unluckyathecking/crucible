import Link from "next/link";

interface PaginationProps {
  basePath: string;
  page: number;
  perPage: number;
  total: number;
  searchParams: Record<string, string | undefined>;
}

export function Pagination({ basePath, page, perPage, total, searchParams }: PaginationProps) {
  const totalPages = Math.max(1, Math.ceil(total / perPage));

  function hrefForPage(target: number): string {
    const params = new URLSearchParams();
    for (const [key, value] of Object.entries(searchParams)) {
      if (value !== undefined && value !== "" && key !== "page") params.set(key, value);
    }
    params.set("page", String(target));
    return `${basePath}?${params.toString()}`;
  }

  return (
    <nav className="flex items-center justify-between mt-4 text-sm" aria-label="Pagination">
      <span className="text-zinc-500 dark:text-zinc-400">
        Page {page} of {totalPages} ({total} total)
      </span>
      <div className="flex gap-2">
        {page > 1 ? (
          <Link href={hrefForPage(page - 1)} className="px-2 py-1 border border-zinc-300 dark:border-zinc-600 rounded">
            Previous
          </Link>
        ) : (
          <span className="px-2 py-1 border border-zinc-200 dark:border-zinc-700 rounded text-zinc-400" aria-disabled="true">
            Previous
          </span>
        )}
        {page < totalPages ? (
          <Link href={hrefForPage(page + 1)} className="px-2 py-1 border border-zinc-300 dark:border-zinc-600 rounded">
            Next
          </Link>
        ) : (
          <span className="px-2 py-1 border border-zinc-200 dark:border-zinc-700 rounded text-zinc-400" aria-disabled="true">
            Next
          </span>
        )}
      </div>
    </nav>
  );
}
