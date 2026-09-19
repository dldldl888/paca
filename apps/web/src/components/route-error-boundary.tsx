import { AlertTriangle, RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";

/**
 * Generic error fallback for TanStack Router route errors (loader failures,
 * render crashes in lazy-loaded route components, etc.).
 *
 * TanStack Router's `autoCodeSplitting` wraps route components in an internal
 * `Lazy` component. When a loader throws or a lazy chunk fails to resolve, the
 * error surfaces as "Element type is invalid… Check the render method of Lazy".
 * Providing an `errorComponent` / `defaultErrorComponent` short-circuits that
 * crash and renders this fallback instead.
 */
export function RouteErrorComponent({ error }: { error: unknown }) {
	const { t } = useTranslation();
	const message = error instanceof Error ? error.message : "";

	const isNotFound =
		message.toLowerCase().includes("not found") ||
		message.toLowerCase().includes("404");

	return (
		<div className="flex flex-col h-full items-center justify-center gap-4 p-6">
			<div className="flex size-12 items-center justify-center rounded-full bg-destructive/10">
				<AlertTriangle className="size-6 text-destructive" />
			</div>
			<div className="text-center space-y-1 max-w-sm">
				<p className="text-sm font-medium text-destructive">
					{isNotFound
						? t("common.notFound", "Not found")
						: t("common.somethingWentWrong", "Something went wrong")}
				</p>
				{message && (
					<p className="text-xs text-muted-foreground wrap-break-word">
						{message}
					</p>
				)}
			</div>
			<Button
				variant="outline"
				size="sm"
				className="gap-1.5"
				onClick={() => window.location.reload()}
			>
				<RefreshCw className="size-3.5" />
				{t("common.retry", "Retry")}
			</Button>
		</div>
	);
}
