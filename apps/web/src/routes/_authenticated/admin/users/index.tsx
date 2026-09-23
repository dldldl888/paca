import { useQuery } from "@tanstack/react-query";
import { createFileRoute, redirect } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { DeleteUserDialog } from "@/components/admin/users/DeleteUserDialog";
import { ResetPasswordDialog } from "@/components/admin/users/ResetPasswordDialog";
import { UserFormDialog } from "@/components/admin/users/UserFormDialog";
import { UsersHeader } from "@/components/admin/users/UsersHeader";
import {
	EmptyUsersState,
	UsersErrorState,
	UsersNoPermissionState,
} from "@/components/admin/users/UsersStates";
import { UsersStats } from "@/components/admin/users/UsersStats";
import { UsersTable } from "@/components/admin/users/UsersTable";
import { UsersTableSkeleton } from "@/components/admin/users/UsersTableSkeleton";
import { Button } from "@/components/ui/button";
import { usePermissions } from "@/hooks/use-permissions";
import {
	myPermissionsQueryOptions,
	type User,
	usersQueryOptions,
} from "@/lib/admin-api";
import { currentUserQueryOptions } from "@/lib/auth-api";
import { hasPermission } from "@/lib/permissions";

export const Route = createFileRoute("/_authenticated/admin/users/")({
	beforeLoad: async ({ context: { queryClient } }) => {
		const permissions = await queryClient
			.fetchQuery(myPermissionsQueryOptions)
			.catch(() => [] as string[]);

		const canAccess =
			hasPermission(permissions, "users.read") ||
			hasPermission(permissions, "users.write") ||
			hasPermission(permissions, "users.delete");

		if (!canAccess) {
			throw redirect({ to: "/home" });
		}
	},
	component: UsersManagementPage,
});

function UsersManagementPage() {
	const { t } = useTranslation("admin");
	const { hasPermission } = usePermissions();
	const canRead = hasPermission("users.read");
	const canWrite = hasPermission("users.write");

	const [page, setPage] = useState(1);
	const pageSize = 20;

	const {
		data: pagedUsers,
		isLoading,
		isError,
		isFetching,
	} = useQuery({ ...usersQueryOptions(page, pageSize), enabled: canRead });

	const { data: currentUser } = useQuery(currentUserQueryOptions);

	const [createOpen, setCreateOpen] = useState(false);
	const [editUser, setEditUser] = useState<User | null>(null);
	const [deleteUser, setDeleteUser] = useState<User | null>(null);
	const [resetPasswordUser, setResetPasswordUser] = useState<User | null>(null);

	const users = pagedUsers?.items ?? [];
	const total = pagedUsers?.total ?? 0;
	const totalPages = Math.max(1, Math.ceil(total / pageSize));
	useEffect(() => {
		if (pagedUsers && !isFetching && page > totalPages) setPage(totalPages);
	}, [pagedUsers, isFetching, page, totalPages]);
	const mustChangePasswordCount = users.filter(
		(u) => u.must_change_password,
	).length;

	return (
		<div className="flex flex-col gap-6 p-6 max-w-5xl w-full mx-auto">
			<UsersHeader canWrite={canWrite} onCreate={() => setCreateOpen(true)} />

			{canWrite ? (
				<UserFormDialog open={createOpen} onOpenChange={setCreateOpen} />
			) : null}

			{canRead && !isLoading && !isError && (
				<UsersStats
					total={total}
					mustChangePasswordCount={mustChangePasswordCount}
				/>
			)}

			{!canRead ? (
				<UsersNoPermissionState />
			) : isLoading ? (
				<UsersTableSkeleton />
			) : isError ? (
				<UsersErrorState />
			) : users.length === 0 ? (
				<EmptyUsersState
					canWrite={canWrite}
					onCreate={() => setCreateOpen(true)}
				/>
			) : (
				<UsersTable
					users={users}
					canWrite={canWrite}
					currentUserId={currentUser?.id}
					onEdit={setEditUser}
					onDelete={setDeleteUser}
					onResetPassword={setResetPasswordUser}
				/>
			)}

			{canRead && (total > pageSize || page > 1) && (
				<nav
					aria-label={t("users.pagination.label")}
					className="flex flex-wrap items-center justify-center gap-3"
				>
					<Button
						variant="outline"
						disabled={page <= 1 || isFetching}
						onClick={() => setPage((current) => Math.max(1, current - 1))}
					>
						{t("users.pagination.previous")}
					</Button>
					{pagedUsers && (
						<span className="text-sm text-muted-foreground" aria-live="polite">
							{t("users.pagination.pageOf", { page, totalPages })}
						</span>
					)}
					<Button
						variant="outline"
						disabled={page >= totalPages || isFetching || isError}
						onClick={() =>
							setPage((current) => Math.min(totalPages, current + 1))
						}
					>
						{t("users.pagination.next")}
					</Button>
				</nav>
			)}

			{editUser ? (
				<UserFormDialog
					user={editUser}
					open={!!editUser}
					onOpenChange={(open) => {
						if (!open) setEditUser(null);
					}}
				/>
			) : null}

			{deleteUser ? (
				<DeleteUserDialog
					user={deleteUser}
					open={!!deleteUser}
					onOpenChange={(open) => {
						if (!open) setDeleteUser(null);
					}}
				/>
			) : null}

			{resetPasswordUser ? (
				<ResetPasswordDialog
					user={resetPasswordUser}
					open={!!resetPasswordUser}
					onOpenChange={(open) => {
						if (!open) setResetPasswordUser(null);
					}}
				/>
			) : null}
		</div>
	);
}
