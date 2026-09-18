import { describe, expect, it } from "vitest";
import { DEFAULT_BRAND_NAME } from "./branding-effects";

describe("employee sidebar branding", () => {
	it("keeps the mobile/collapsed brand readable without a supplied logo", () => {
		expect(DEFAULT_BRAND_NAME).toBe("芯鲜派");
	});
});
