import { useTranslation } from "react-i18next";

import type { GlobalRole, User } from "@/lib/admin-api";

interface OrganizationScopeSummaryProps {
	users: User[];
	totalUsers: number;
	roles: GlobalRole[];
}

export function OrganizationScopeSummary({
	users,
	totalUsers,
	roles,
}: OrganizationScopeSummaryProps) {
	const { t } = useTranslation("admin");
	const assignedByRole = new Map<string, number>();
	for (const user of users) {
		assignedByRole.set(user.role, (assignedByRole.get(user.role) ?? 0) + 1);
	}

	return (
		<section
			aria-label={t("users.organizationScope.title")}
			className="rounded-xl border bg-muted/20 p-5"
		>
			<div className="flex flex-wrap items-start justify-between gap-3">
				<div>
					<h2 className="font-semibold">
						{t("users.organizationScope.title")}
					</h2>
					<p className="mt-1 text-sm text-muted-foreground">
						{t("users.organizationScope.description")}
					</p>
				</div>
				<p className="text-sm text-muted-foreground">
					{t("users.organizationScope.coverage", {
						shown: users.length,
						total: totalUsers,
					})}
				</p>
			</div>

			{roles.length > 0 ? (
				<div className="mt-4 grid gap-3 sm:grid-cols-2">
					{roles.map((role) => {
						const permissions = Object.entries(role.permissions)
							.filter(([, granted]) => granted)
							.map(([permission]) => permission);
						return (
							<div
								key={role.id}
								className="rounded-lg border bg-background p-3"
							>
								<div className="flex flex-wrap items-center justify-between gap-2">
									<span className="font-mono text-sm font-medium">
										{role.name}
									</span>
									<span className="text-xs text-muted-foreground">
										{t("users.organizationScope.assigned", {
											count: assignedByRole.get(role.name) ?? 0,
										})}
									</span>
								</div>
								<p className="mt-2 text-xs text-muted-foreground">
									{permissions.length > 0
										? permissions.join(", ")
										: t("users.organizationScope.noPermissions")}
								</p>
							</div>
						);
					})}
				</div>
			) : null}

			<div className="mt-4 border-t pt-3 text-xs text-muted-foreground">
				<p className="font-medium text-foreground">
					{t("users.organizationScope.departmentsPending")}
				</p>
				<p className="mt-1">
					{t("users.organizationScope.departmentsPendingDescription")}
				</p>
				<p className="mt-1">{t("users.organizationScope.projectRolesNote")}</p>
			</div>
		</section>
	);
}
