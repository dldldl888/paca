import { type BrowserContext, expect, test } from "@playwright/test";
import { EmployeeWorkbenchPage } from "../pages/employee-workbench.page";
import { fixture as f } from "../workbench.config";

const taskPath = `/projects/${f.departmentId}/tasks/${f.taskId}`;
const taskApi = `/api/v1${taskPath}`;
const privateGoalMarkers =
	/PRIVATE-MANAGER-CONTEXT-|PRIVATE_MANAGER_GOAL|INTERNAL_SYSTEM_PROMPT/;

async function authenticate(
	context: BrowserContext,
	username: string,
	password: string,
) {
	await context.clearCookies();
	const response = await context.request.post(
		"http://127.0.0.1:3400/api/v1/auth/login",
		{
			data: { username, password, remember_me: true },
		},
	);
	expect(response.status()).toBe(200);
}

test.describe("Employee workbench acceptance", () => {
	test.beforeEach(async ({ context }) => {
		await authenticate(context, f.employeeUsername, f.employeePassword);
	});
	test("keeps the branded home and mobile composer within the viewport", async ({
		page,
	}, testInfo) => {
		const workbench = new EmployeeWorkbenchPage(page);
		await workbench.home();
		const logo = page.getByRole("img", { name: "芯鲜派", exact: true });
		await expect(logo).toBeVisible();
		await expect(logo).toHaveAttribute("src", "/brand/xianxianpai-logo.png");
		expect(
			await logo.evaluate(
				(image: HTMLImageElement) => image.complete && image.naturalWidth > 0,
			),
		).toBe(true);
		await page.setViewportSize({ width: 390, height: 844 });
		await expect(
			page.getByRole("region", { name: "芯鲜派员工工作台" }),
		).toBeVisible();
		expect(
			await page.evaluate(() => document.documentElement.scrollWidth),
		).toBeLessThanOrEqual(390);
		await page.screenshot({
			path: testInfo.outputPath("mobile-home.png"),
			fullPage: true,
		});
		await workbench.conversation(f.conversationId);
		await page.getByRole("button", { name: "切换侧边栏", exact: true }).click();
		const drawer = page.getByRole("dialog");
		await expect(drawer).toBeVisible();
		const mobileLogo = drawer.getByRole("img", { name: "芯鲜派", exact: true });
		await expect(mobileLogo).toBeVisible();
		const drawerBox = await drawer.boundingBox();
		const logoBox = await mobileLogo.boundingBox();
		if (!drawerBox || !logoBox)
			throw new Error("Mobile drawer or logo has no visible geometry");
		expect(logoBox.x).toBeGreaterThanOrEqual(drawerBox.x);
		expect(logoBox.x + logoBox.width).toBeLessThanOrEqual(
			Math.min(390, drawerBox.x + drawerBox.width),
		);
		expect(logoBox.y).toBeGreaterThanOrEqual(drawerBox.y);
		expect(logoBox.y + logoBox.height).toBeLessThanOrEqual(
			drawerBox.y + drawerBox.height,
		);
		const originalAspect = await mobileLogo.evaluate(
			(image: HTMLImageElement) => image.naturalWidth / image.naturalHeight,
		);
		expect(logoBox.width / logoBox.height).toBeCloseTo(originalAspect, 2);
		await page.screenshot({
			path: testInfo.outputPath("mobile-sidebar-logo.png"),
			fullPage: true,
		});
		await drawer.getByRole("link", { name: "芯鲜派", exact: true }).click();
		await expect(page).toHaveURL(/\/home$/);
		await page.keyboard.press("Escape");
		await expect(drawer).toHaveCount(0);
		await workbench.conversation(f.conversationId);
		const composer = page.getByRole("textbox", { name: "消息输入框" });
		const box = await composer.boundingBox();
		if (!box) throw new Error("Mobile composer has no visible geometry");
		expect(box.width).toBeGreaterThanOrEqual(300);
		expect(box.x + box.width).toBeLessThanOrEqual(390);
		await page.getByRole("button", { name: "打开对话历史" }).click();
		await expect(
			page.getByRole("button", { name: "关闭对话历史" }),
		).toBeVisible();
		expect(
			await page
				.locator('textarea[aria-label="消息输入框"]')
				.evaluate((element) => element.closest("[inert]") !== null),
		).toBe(true);
		await expect(
			page.getByRole("button", { name: "关闭对话历史" }),
		).toBeFocused();
		await page.keyboard.press("Enter");
		await expect(
			page.getByRole("button", { name: "打开对话历史" }),
		).toBeFocused();
		await expect(composer).toBeVisible();
		expect(
			await composer.evaluate((element) => element.closest("[inert]") !== null),
		).toBe(false);
		await page.screenshot({
			path: testInfo.outputPath("mobile-conversation.png"),
			fullPage: true,
		});
	});

	test("does not expose another department through navigation or direct requests", async ({
		page,
	}) => {
		const workbench = new EmployeeWorkbenchPage(page);
		await workbench.home();
		await expect(
			page.locator(`a[href="/projects/${f.departmentId}"]`),
		).toBeVisible();
		await expect(page.locator(`a[href*="${f.otherDepartmentId}"]`)).toHaveCount(
			0,
		);
		const forbidden = await page.request.get(
			`/api/v1/projects/${f.otherDepartmentId}`,
		);
		expect(forbidden.status()).toBe(403);
		await page.goto(`/projects/${f.otherDepartmentId}`);
		await expect(page).toHaveURL(/\/home$/);
		await expect(page.locator(`a[href*="${f.otherDepartmentId}"]`)).toHaveCount(
			0,
		);
	});

	test("hides manager entries and rejects employee draft access", async ({
		page,
		browser,
	}) => {
		const workbench = new EmployeeWorkbenchPage(page);
		await workbench.home();
		await expect(
			page.getByRole("link", {
				name: /团队目标与草稿|从任务查看反馈|管理工作区/,
			}),
		).toHaveCount(0);
		await expect(page.locator('a[href$="/drafts"]')).toHaveCount(0);
		const response = await page.request.get(
			`/api/v1/projects/${f.departmentId}/drafts`,
		);
		expect(response.status()).toBe(403);
		await page.goto(`/projects/${f.departmentId}/drafts`);
		await expect(
			page.getByText(/没有权限|无权|仅.*管理|不可用|无法|失败/).first(),
		).toBeVisible();
		await expect(
			page.locator(
				'a[href$="/agents"], a[href$="/environments"], a[href$="/automation"], a[href="/admin/settings"], a[href="/admin/plugins"]',
			),
		).toHaveCount(0);
		await page
			.getByRole("button", { name: new RegExp(f.assigneeName) })
			.focus();
		await page.keyboard.press("Enter");
		await expect(
			page.getByRole("menuitem", { name: /个人资料/ }),
		).toBeVisible();
		await expect(
			page.getByRole("menuitem", { name: /API 密钥|桥接|开发者|插件|环境/ }),
		).toHaveCount(0);
		const admin = await browser.newContext({
			locale: "zh-CN",
		});
		try {
			await authenticate(admin, f.adminUsername, f.adminPassword);
			const adminPage = await admin.newPage();
			await adminPage.goto("http://127.0.0.1:3400/home");
			await adminPage
				.locator('[data-sidebar="menu-button"][aria-haspopup="menu"]')
				.focus();
			await adminPage.keyboard.press("Enter");
			await expect(
				adminPage.getByRole("menuitem", { name: /API 密钥/ }),
			).toBeVisible();
			await adminPage.keyboard.press("Escape");
			await expect(
				adminPage.locator('a[href="/admin/settings"]'),
			).toBeVisible();
			await adminPage.locator('a[href="/admin/settings"]').click();
			await expect(adminPage).toHaveURL(/\/admin\/settings$/);
			await adminPage.goto(`http://127.0.0.1:3400/projects/${f.departmentId}`);
			await expect(
				adminPage.locator(`a[href="/projects/${f.departmentId}/agents"]`),
			).toBeVisible();
			await expect(
				adminPage.locator(`a[href="/projects/${f.departmentId}/environments"]`),
			).toBeVisible();
		} finally {
			await admin.close();
		}
	});

	test("opens the assigned notification with requirements criteria assignee and deadline", async ({
		page,
	}) => {
		const workbench = new EmployeeWorkbenchPage(page);
		await workbench.openNotification(f.taskTitle);
		await expect(page).toHaveURL(new RegExp(`${f.taskId}$`));
		await workbench.assertTaskFields(f);
		const response = await page.request.get(taskApi);
		expect(response.ok()).toBe(true);
		const task = (await response.json()).data;
		expect(task.title).toBe(f.taskTitle);
		expect(task.assignee_ids).toHaveLength(1);
		expect(task.due_date.slice(0, 10)).toBe(f.dueDate);
	});

	test("persists employee status and feedback for a fresh manager without private context", async ({
		page,
		browser,
	}) => {
		const workbench = new EmployeeWorkbenchPage(page);
		await workbench.openNotification(f.taskTitle);
		const original = (await (await page.request.get(taskApi)).json()).data;
		const feedback = `Playwright 合成验收反馈 ${Date.now()}：12份样本清单完整，已提交经理复核。`;
		try {
			await workbench.status("Done", "In Progress");
			await workbench.feedback(feedback);
			await workbench.status("In Progress", "Done");
			await page.reload();
			await expect(
				page.getByRole("button", { name: "Done", exact: true }),
			).toBeVisible();
			await expect(page.getByText(feedback, { exact: true })).toBeVisible();
			await expect(page.getByRole("main")).not.toContainText(
				privateGoalMarkers,
			);
			const employeeTask = await page.request.get(taskApi);
			const employeeActivities = await page.request.get(
				`${taskApi}/activities`,
			);
			expect(employeeTask.ok()).toBe(true);
			expect(employeeActivities.ok()).toBe(true);
			expect(await employeeTask.text()).not.toMatch(privateGoalMarkers);
			expect(await employeeActivities.text()).not.toMatch(privateGoalMarkers);
			const manager = await browser.newContext({
				locale: "zh-CN",
			});
			try {
				await authenticate(manager, f.managerUsername, f.managerPassword);
				const managerPage = await manager.newPage();
				await managerPage.goto(`http://127.0.0.1:3400${taskPath}`);
				await expect(
					managerPage.getByRole("button", { name: "Done", exact: true }),
				).toBeVisible();
				await expect(
					managerPage.getByText(feedback, { exact: true }),
				).toBeVisible();
				await expect(managerPage.getByRole("main")).not.toContainText(
					f.originalHistory,
				);
				await expect(managerPage.getByRole("main")).not.toContainText(
					privateGoalMarkers,
				);
				const managerTask = await manager.request.get(
					`http://127.0.0.1:3400${taskApi}`,
				);
				const managerActivities = await manager.request.get(
					`http://127.0.0.1:3400${taskApi}/activities`,
				);
				expect(managerTask.ok()).toBe(true);
				expect(managerActivities.ok()).toBe(true);
				expect(await managerTask.text()).not.toMatch(privateGoalMarkers);
				expect(await managerActivities.text()).not.toMatch(privateGoalMarkers);
				const privateConversation = await manager.request.get(
					`http://127.0.0.1:3400/api/v1/agents/conversations/${f.conversationId}`,
				);
				expect([403, 404]).toContain(privateConversation.status());
			} finally {
				await manager.close();
			}
		} finally {
			const restored = await page.request.patch(taskApi, {
				data: { status_id: original.status_id },
			});
			expect(restored.ok()).toBe(true);
		}
	});

	test("completes employee task loop and preserves private assistant history", async ({
		page,
		browser,
	}, testInfo) => {
		const workbench = new EmployeeWorkbenchPage(page);
		await workbench.login(f.employeeUsername, f.employeePassword);
		await expect(
			page.getByRole("img", { name: "芯鲜派", exact: true }),
		).toBeVisible();
		await page.getByRole("link", { name: "首页", exact: true }).focus();
		await expect(
			page.getByRole("link", { name: "首页", exact: true }),
		).toBeFocused();
		await workbench.openNotification(f.taskTitle);
		await workbench.assertTaskFields(f);
		await expect(
			page.getByRole("button", { name: "Done", exact: true }),
		).toBeVisible();
		const feedback = `Playwright 全流程合成反馈 ${Date.now()}：任务要求与完成标准已核对。`;
		try {
			await workbench.status("Done", "In Progress");
			await workbench.feedback(feedback);
			await workbench.status("In Progress", "Done");
		} finally {
			const statuses = (
				await (
					await page.request.get(
						`/api/v1/projects/${f.departmentId}/task-statuses`,
					)
				).json()
			).data.items;
			const restored = await page.request.patch(taskApi, {
				data: {
					status_id: statuses.find(
						(status: { name: string }) => status.name === "Done",
					).id,
				},
			});
			expect(restored.ok()).toBe(true);
		}
		await workbench.conversation(f.conversationId);
		await expect(
			page.getByText(f.originalHistory, { exact: true }),
		).toBeVisible();
		const composer = page.getByRole("textbox", { name: "消息输入框" });
		const marker = `Playwright 合成对话 ${Date.now()}`;
		await composer.fill(
			`${marker}：请连续写1000条不同的简短问候语。只输出文字，不调用任何工具，不读取或修改任何资料。`,
		);
		await page.getByRole("button", { name: "发送消息" }).focus();
		await page.keyboard.press("Enter");
		const stop = page.getByRole("button", { name: "停止生成" });
		await expect(stop).toBeEnabled({ timeout: 45_000 });
		await stop.focus();
		await expect(stop).toBeFocused();
		await page.keyboard.press("Enter");
		await expect(
			page
				.getByText("聊天会话", { exact: true })
				.locator("..")
				.getByText("Stopped", { exact: true }),
		).toBeVisible({ timeout: 45_000 });
		await expect(composer).toBeEnabled();
		const reply = `续聊验收通过${Date.now()}`;
		await composer.fill(`请只回复“${reply}”。不要调用工具或修改资料。`);
		await page.getByRole("button", { name: "发送消息" }).click();
		await expect(page.getByText(reply, { exact: true })).toBeVisible({
			timeout: 90_000,
		});
		await expect(page.getByRole("button", { name: "停止生成" })).toHaveCount(
			0,
			{ timeout: 30_000 },
		);
		await page.reload();
		await expect(
			page.getByText(f.originalHistory, { exact: true }),
		).toBeVisible();
		await expect(page.getByText(reply, { exact: true })).toBeVisible();
		await page.setViewportSize({ width: 390, height: 844 });
		await workbench.login(f.employeeUsername, f.employeePassword);
		await workbench.openNotification(f.taskTitle);
		await workbench.assertTaskFields(f);
		await expect(page.getByText(feedback, { exact: true })).toBeVisible();
		await workbench.conversation(f.conversationId);
		await page.getByRole("button", { name: "打开对话历史" }).click();
		await expect(
			page.getByRole("button", { name: "关闭对话历史" }),
		).toBeFocused();
		await page.keyboard.press("Enter");
		await expect(
			page.getByRole("button", { name: "打开对话历史" }),
		).toBeFocused();
		await expect(page.getByText(reply, { exact: true })).toBeVisible();
		const manager = await browser.newContext({
			locale: "zh-CN",
		});
		try {
			await authenticate(manager, f.managerUsername, f.managerPassword);
			const managerPage = await manager.newPage();
			await managerPage.goto("http://127.0.0.1:3400/home");
			const drafts = managerPage.getByRole("link", { name: /^团队目标与草稿/ });
			await expect(drafts).toBeVisible();
			await drafts.focus();
			await managerPage.keyboard.press("Enter");
			await expect(managerPage).toHaveURL(
				new RegExp(`/projects/${f.departmentId}/drafts$`),
			);
			await managerPage.setViewportSize({ width: 390, height: 844 });
			await managerPage.goto("http://127.0.0.1:3400/home");
			await expect(
				managerPage.getByRole("link", { name: /^团队目标与草稿/ }),
			).toBeVisible();
			await managerPage.goto(`http://127.0.0.1:3400${taskPath}`);
			await expect(
				managerPage.getByRole("heading", { name: f.taskTitle, exact: true }),
			).toBeVisible();
			await expect(
				managerPage.getByText(feedback, { exact: true }),
			).toBeVisible();
			await expect(managerPage.getByRole("main")).not.toContainText(marker);
			const privateConversation = await manager.request.get(
				`http://127.0.0.1:3400/api/v1/agents/conversations/${f.conversationId}`,
			);
			expect([403, 404]).toContain(privateConversation.status());
		} finally {
			await manager.close();
		}
		await page.screenshot({
			path: testInfo.outputPath("assistant-history.png"),
			fullPage: true,
		});
	});
});
