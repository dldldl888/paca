import { expect, test } from "bun:test";
import { loadConfig } from "./config.ts";

test("requires one stable nonzero deployment instance ID", () => {
    const saved = { ...process.env };
    try {
        process.env.API_URL = "http://localhost:8080";
        process.env.REDIS_URL = "redis://localhost:6379";
        process.env.INTERNAL_API_KEY = "synthetic-test-key";
        for (const value of ["", "not-a-uuid", "00000000-0000-0000-0000-000000000000"]) {
            process.env.PACA_INSTANCE_ID = value;
            expect(() => loadConfig()).toThrow("PACA_INSTANCE_ID");
        }
        process.env.PACA_INSTANCE_ID = "11111111-2222-4333-8444-555555555555";
        expect(loadConfig().instanceID).toBe(process.env.PACA_INSTANCE_ID);
    } finally {
        process.env = saved;
    }
});
