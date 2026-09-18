import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Bell, BookOpen, MessageCircle } from "lucide-react";
import { useTranslation } from "react-i18next";
import { NotificationBell } from "@/components/app-shell/notification-bell";
import { OrganizationNavigation } from "@/components/company/organization-navigation";
import { AssignedTasksList } from "@/components/home/assigned-tasks-list";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { chattableAgentsQueryOptions } from "@/lib/agent-api";
import { currentUserQueryOptions } from "@/lib/auth-api";
import { docListQueryOptions } from "@/lib/doc-api";
import { notificationsQueryOptions } from "@/lib/notification-api";
import { projectsQueryOptions } from "@/lib/project-api";

export function XianxianpaiHome() {
	const { t } = useTranslation("shared");
	const { data: user } = useQuery(currentUserQueryOptions);
	const {
		data: projects,
		isPending: projectsPending,
		isError: projectsError,
	} = useQuery(projectsQueryOptions());
	const {
		data: notifications,
		isPending: notificationsPending,
		isError: notificationsError,
	} = useInfiniteQuery(notificationsQueryOptions);
	const {
		data: chattableAgents,
		isPending: chattableAgentsPending,
		isError: chattableAgentsError,
	} = useQuery(chattableAgentsQueryOptions);

	const displayName = user?.full_name || user?.username;
	const companyProject = projects?.items.find(
		(project) => project.name === "公司资料" || project.name === "总经办",
	);
	const {
		data: companyDocuments,
		isPending: companyDocumentsPending,
		isError: companyDocumentsError,
	} = useQuery({
		...docListQueryOptions(companyProject?.id ?? ""),
		enabled: !!companyProject && !projectsPending && !projectsError,
	});
	const unreadCount = notifications?.pages[0]?.unread_count ?? 0;
	const xiaopaiOffline = chattableAgentsError;
	const xiaopaiUnavailable =
		!chattableAgentsPending &&
		!chattableAgentsError &&
		chattableAgents?.length === 0;
	const hasNotifications = (notifications?.pages ?? []).some(
		(page) => page.items.length > 0,
	);

	return (
		<section
			aria-label={t("home.workbench.title")}
			className="flex flex-col gap-4 border-b border-border/50 bg-muted/10 px-6 py-6"
		>
			<Card className="border-border/60">
				<CardHeader className="pb-2">
					<CardTitle className="font-[Syne] text-xl">
						{displayName
							? t("home.workbench.welcome", { name: displayName })
							: t("home.workbench.welcomeFallback")}
					</CardTitle>
				</CardHeader>
				<CardContent className="pt-0 text-sm text-muted-foreground">
					{t("home.workbench.subtitle")}
				</CardContent>
			</Card>

			<section aria-labelledby="xianxianpai-home-tasks">
				<h2
					id="xianxianpai-home-tasks"
					className="mb-2 text-base font-semibold"
				>
					{t("home.workbench.tasks")}
				</h2>
				<AssignedTasksList />
			</section>

			<Card className="border-border/60">
				<CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
					<CardTitle className="flex items-center gap-2 text-base">
						<Bell className="size-4" />
						{t("home.workbench.notifications")}
					</CardTitle>
					<NotificationBell />
				</CardHeader>
				<CardContent className="pt-0 text-sm text-muted-foreground">
					{notificationsPending
						? t("home.workbench.notificationsLoading")
						: notificationsError
							? t("home.workbench.notificationsOffline")
							: hasNotifications
								? t("home.workbench.unreadNotifications", {
										count: unreadCount,
									})
								: t("home.workbench.notificationsEmpty")}
				</CardContent>
			</Card>

			<div className="grid gap-4 sm:grid-cols-2">
				<Card className="border-border/60 transition-colors hover:border-primary/40">
					<Link to="/conversations" className="block h-full">
						<CardHeader className="pb-2">
							<CardTitle className="flex items-center gap-2 text-base">
								<MessageCircle className="size-4 text-primary" />
								{t("home.workbench.xiaopai")}
							</CardTitle>
						</CardHeader>
						<CardContent className="pt-0 text-sm text-muted-foreground">
							{chattableAgentsPending
								? t("home.workbench.xiaopaiLoading")
								: xiaopaiOffline
									? t("home.workbench.xiaopaiOffline")
									: xiaopaiUnavailable
										? t("home.workbench.xiaopaiUnavailable")
										: t("home.workbench.xiaopaiDescription")}
						</CardContent>
					</Link>
				</Card>

				<Card className="border-border/60 transition-colors hover:border-primary/40">
					{projectsPending || (companyProject && companyDocumentsPending) ? (
						<CardContent className="pt-0 text-sm text-muted-foreground">
							{t("home.workbench.companyDocsLoading")}
						</CardContent>
					) : projectsError || (companyProject && companyDocumentsError) ? (
						<CardContent className="pt-0 text-sm text-muted-foreground">
							{t("home.workbench.companyDocsError")}
						</CardContent>
					) : companyProject && companyDocuments?.[0] ? (
						<Link
							to="/projects/$projectId/docs/$docId"
							params={{
								projectId: companyProject.id,
								docId: companyDocuments[0].id,
							}}
							className="block h-full"
						>
							<CardHeader className="pb-2">
								<CardTitle className="flex items-center gap-2 text-base">
									<BookOpen className="size-4 text-primary" />
									{t("home.workbench.companyDocs")}
								</CardTitle>
							</CardHeader>
							<CardContent className="pt-0 text-sm text-muted-foreground">
								{t("home.workbench.companyDocsDescription")}
							</CardContent>
						</Link>
					) : (
						<div className="h-full">
							<CardHeader className="pb-2">
								<CardTitle className="flex items-center gap-2 text-base">
									<BookOpen className="size-4 text-muted-foreground" />
									{t("home.workbench.companyDocs")}
								</CardTitle>
							</CardHeader>
							<CardContent className="pt-0 text-sm text-muted-foreground">
								{t("home.workbench.companyDocsEmpty")}
							</CardContent>
						</div>
					)}
				</Card>
			</div>

			<OrganizationNavigation />
		</section>
	);
}
