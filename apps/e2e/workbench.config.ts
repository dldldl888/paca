import { readFileSync } from "node:fs";
import { defineConfig } from "@playwright/test";

// Deliberately independent of the default suite's login and destructive cleanup.
if (process.env.E2E_BASE_URL !== "http://127.0.0.1:3400") {
	throw new Error(
		"Workbench acceptance requires E2E_BASE_URL=http://127.0.0.1:3400",
	);
}
if (!process.env.E2E_WORKBENCH_FIXTURE) {
	throw new Error(
		"E2E_WORKBENCH_FIXTURE must identify a dedicated synthetic fixture JSON file",
	);
}
export const fixture = JSON.parse(
	readFileSync(process.env.E2E_WORKBENCH_FIXTURE, "utf8"),
);
for (const key of [
	"employeeUsername",
	"employeePassword",
	"managerUsername",
	"managerPassword",
	"adminUsername",
	"adminPassword",
	"departmentId",
	"otherDepartmentId",
	"taskId",
	"taskTitle",
	"conversationId",
	"originalHistory",
	"requirements",
	"criteria",
	"assigneeName",
	"dueDateLabel",
	"dueDate",
]) {
	if (typeof fixture[key] !== "string" || !fixture[key])
		throw new Error(`Missing fixture field: ${key}`);
}
if (fixture.synthetic !== true)
	throw new Error("Fixture must explicitly attest synthetic=true");

export default defineConfig({
	testDir: "./workbench",
	fullyParallel: false,
	workers: 1,
	retries: 0,
	timeout: 180_000,
	expect: { timeout: 15_000 },
	outputDir:
		process.env.E2E_WORKBENCH_OUTPUT ?? "../../output/playwright/workbench",
	reporter: [["list"]],
	use: {
		baseURL: process.env.E2E_BASE_URL,
		locale: "zh-CN",
		viewport: { width: 1440, height: 1000 },
		// Login uses private fixture credentials; do not record them in traces.
		trace: "off",
		screenshot: "only-on-failure",
		actionTimeout: 15_000,
	},
	projects: [
		{ name: "chromium", use: { browserName: "chromium", channel: "chrome" } },
	],
});
