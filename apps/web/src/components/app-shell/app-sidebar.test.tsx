import { describe, expect, it } from "vitest";
import { canShowProjectNavItem } from "./app-sidebar";
import { DEFAULT_BRAND_NAME } from "./branding-effects";

describe("employee sidebar branding", () => {
	it("keeps the mobile/collapsed brand readable without a supplied logo", () => {
		expect(DEFAULT_BRAND_NAME).toBe("芯鲜派");
	});
});

describe("project management navigation", () => {
	it("hides technical management pages for an ordinary member", () => {
		const ordinaryMember = {
			canManageDrafts: false,
			canManageProject: false,
			canManageAgents: true,
			canManageEnvironments: true,
			canManageWorkflows: true,
		};

		expect(canShowProjectNavItem("agents", ordinaryMember)).toBe(false);
		expect(canShowProjectNavItem("environments", ordinaryMember)).toBe(false);
		expect(canShowProjectNavItem("automation", ordinaryMember)).toBe(false);
	});
});
