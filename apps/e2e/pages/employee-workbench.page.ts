import { expect, type Page } from "@playwright/test";

/** Real employee UI; request reads verify persistence without replacing responses. */
export class EmployeeWorkbenchPage {
	constructor(readonly page: Page) {}

	async login(username: string, password: string) {
		await this.page.context().clearCookies();
		await this.page.goto("/");
		await this.page
			.getByRole("textbox", { name: "用户名", exact: true })
			.fill(username);
		await this.page
			.getByRole("textbox", { name: "密码", exact: true })
			.fill(password);
		await this.page.getByRole("button", { name: "登录", exact: true }).focus();
		await this.page.keyboard.press("Enter");
		await expect(this.page).toHaveURL(/\/home$/);
	}

	async home() {
		await this.page.goto("/home");
		await expect(
			this.page.getByRole("region", { name: "芯鲜派员工工作台" }),
		).toBeVisible();
	}

	async openNotification(taskTitle: string) {
		await this.home();
		await this.page
			.getByRole("region", { name: "芯鲜派员工工作台" })
			.getByRole("button", { name: "通知", exact: true })
			.focus();
		await this.page.keyboard.press("Enter");
		await this.page
			.getByRole("button", {
				name: new RegExp(taskTitle.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")),
			})
			.first()
			.click();
		await expect(
			this.page.getByRole("heading", { name: taskTitle, exact: true }),
		).toBeVisible();
	}

	async assertTaskFields(f: {
		requirements: string;
		criteria: string;
		assigneeName: string;
		dueDateLabel: string;
	}) {
		await expect(
			this.page.getByText(f.requirements, { exact: true }),
		).toBeVisible();
		await expect(
			this.page.getByText(f.criteria, { exact: true }),
		).toBeVisible();
		await expect(this.page.getByRole("main")).toContainText(f.assigneeName);
		await expect(
			this.page.getByRole("button", { name: f.dueDateLabel, exact: true }),
		).toBeVisible();
	}

	async status(from: string, to: string) {
		await this.page.getByRole("button", { name: from, exact: true }).focus();
		await this.page.keyboard.press("Enter");
		await this.page
			.getByRole("dialog")
			.getByRole("button", { name: to, exact: true })
			.click();
		await this.page.keyboard.press("Escape");
		await expect(this.page.getByRole("dialog")).toHaveCount(0);
		await expect(
			this.page.getByRole("button", { name: to, exact: true }),
		).toBeVisible();
	}

	async feedback(text: string) {
		const editor = this.page.locator("fieldset [contenteditable=true]");
		await editor.fill(text);
		const saved = this.page.waitForResponse(
			(response) =>
				response.url().endsWith("/activities/comments") &&
				response.request().method() === "POST",
		);
		await editor.press("Control+Enter");
		expect((await saved).ok()).toBe(true);
		await expect(this.page.getByText(text, { exact: true })).toBeVisible();
	}

	async conversation(id: string) {
		await this.page.goto(`/conversations/${id}`);
		await expect(
			this.page.getByRole("textbox", { name: "消息输入框" }),
		).toBeVisible();
	}
}
