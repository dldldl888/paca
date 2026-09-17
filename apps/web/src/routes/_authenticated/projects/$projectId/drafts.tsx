import { createFileRoute } from "@tanstack/react-router";
import { DraftWorkspace } from "@/components/projects/drafts/draft-workspace";

export const Route = createFileRoute(
	"/_authenticated/projects/$projectId/drafts",
)({
	validateSearch: (search: Record<string, unknown>): { draft?: string } => ({
		draft: typeof search.draft === "string" ? search.draft : undefined,
	}),
	component: DraftPage,
});

function DraftPage() {
	const { projectId } = Route.useParams();
	const { draft } = Route.useSearch();
	const navigate = Route.useNavigate();
	return (
		<DraftWorkspace
			key={`${projectId}:${draft ?? "new"}`}
			projectId={projectId}
			draftId={draft}
			onSelectDraft={(id) => void navigate({ search: { draft: id } })}
		/>
	);
}
