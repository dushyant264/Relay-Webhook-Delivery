import path from "node:path";
import Fastify from "fastify";
import fastifyStatic from "@fastify/static";
import { collectDefaultMetrics, register } from "prom-client";
import { ZodError } from "zod";
import { pool } from "./db.js";
import { tenantRoutes } from "./routes/tenants.js";
import { applicationRoutes } from "./routes/applications.js";
import { endpointRoutes } from "./routes/endpoints.js";
import { apiKeyRoutes } from "./routes/apiKeys.js";
import { deliveryRoutes } from "./routes/deliveries.js";
import { overviewRoutes } from "./routes/overview.js";

collectDefaultMetrics();

const app = Fastify({ logger: true });

const ADMIN_TOKEN = process.env.ADMIN_TOKEN ?? "dev-admin-token";

// Demo-grade admin auth: a single bearer token gates the management API.
// Static dashboard assets, /healthz and /metrics stay public — the dashboard
// itself asks for the token and sends it on every /v1 call.
app.addHook("onRequest", async (req, reply) => {
  if (!req.url.startsWith("/v1")) return;
  const token = req.headers.authorization?.replace(/^Bearer /, "");
  if (token !== ADMIN_TOKEN) {
    return reply.code(401).send({ error: "invalid admin token" });
  }
});

// Dashboard SPA (control-plane/public).
await app.register(fastifyStatic, {
  root: path.join(import.meta.dirname, "..", "public"),
});

app.setErrorHandler((err, req, reply) => {
  if (err instanceof ZodError) {
    return reply.code(400).send({ error: "validation failed", issues: err.issues });
  }
  req.log.error(err);
  return reply.code(500).send({ error: "internal error" });
});

app.get("/healthz", async () => ({ ok: true }));
app.get("/metrics", async (_req, reply) => {
  reply.header("Content-Type", register.contentType);
  return register.metrics();
});

await app.register(tenantRoutes, { prefix: "/v1" });
await app.register(applicationRoutes, { prefix: "/v1" });
await app.register(endpointRoutes, { prefix: "/v1" });
await app.register(apiKeyRoutes, { prefix: "/v1" });
await app.register(deliveryRoutes, { prefix: "/v1" });
await app.register(overviewRoutes, { prefix: "/v1" });

const port = Number(process.env.PORT ?? 8080);
try {
  await app.listen({ port, host: "0.0.0.0" });
} catch (err) {
  app.log.error(err);
  await pool.end();
  process.exit(1);
}

for (const sig of ["SIGINT", "SIGTERM"] as const) {
  process.on(sig, async () => {
    await app.close();
    await pool.end();
    process.exit(0);
  });
}
