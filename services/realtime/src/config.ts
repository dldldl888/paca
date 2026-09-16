// Configuration loaded from environment variables.
// All required variables throw at startup if absent so misconfiguration is
// caught immediately rather than at first use.

export interface Config {
	port: number;
	apiUrl: string;
 internalAPIKey: string;
 instanceID: string;
	valkey: {
		url: string;
	};
	cors: {
		origins: string[];
	};
	logLevel: string;
}

function requireEnv(name: string): string {
	const value = process.env[name];
	if (!value) throw new Error(`Missing required environment variable: ${name}`);
	return value;
}

export function loadConfig(): Config {
	return {
		port: parseInt(process.env.PORT ?? "3001", 10),
		// Internal API base URL (service-to-service, not via the public gateway).
		apiUrl: requireEnv("API_URL"),
 internalAPIKey: requireEnv("INTERNAL_API_KEY"),
 instanceID: requireEnv("PACA_INSTANCE_ID"),
		valkey: {
			url: requireEnv("REDIS_URL"),
		},
		cors: {
			// Comma-separated list of allowed origins, e.g. "http://localhost:3000,https://app.example.com"
			origins: (process.env.CORS_ORIGINS ?? "http://localhost:3000")
				.split(",")
				.map((s) => s.trim())
				.filter(Boolean),
		},
		logLevel: process.env.LOG_LEVEL ?? "info",
	};
}
