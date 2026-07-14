import type { FastifyInstance } from "fastify";
import { z } from "zod";
import { query, queryOne } from "../db.js";

const createApplication = z.object({
  tenant_id: z.string().uuid(),
  name: z.string().min(1).max(120),
});

const createEventType = z.object({
  name: z
    .string()
    .min(1)
    .max(120)
    .regex(/^[a-z0-9_.-]+$/, "lowercase dotted names, e.g. invoice.paid"),
  description: z.string().max(500).optional(),
});

export async function applicationRoutes(app: FastifyInstance) {
  app.post("/applications", async (req, reply) => {
    const body = createApplication.parse(req.body);
    const row = await queryOne(
      `insert into applications (tenant_id, name) values ($1, $2)
       on conflict (tenant_id, name) do nothing
       returning id, tenant_id, name, created_at`,
      [body.tenant_id, body.name],
    );
    if (!row) return reply.code(409).send({ error: "application already exists for tenant" });
    return reply.code(201).send(row);
  });

  app.get("/applications", async () => {
    return query(
      `select a.id, a.tenant_id, t.name as tenant_name, a.name, a.created_at
       from applications a join tenants t on t.id = a.tenant_id
       order by a.created_at`,
    );
  });

  app.post<{ Params: { appId: string } }>(
    "/applications/:appId/event-types",
    async (req, reply) => {
      const body = createEventType.parse(req.body);
      const row = await queryOne(
        `insert into event_types (application_id, name, description) values ($1, $2, $3)
         on conflict (application_id, name) do nothing
         returning id, application_id, name, description, created_at`,
        [req.params.appId, body.name, body.description ?? null],
      );
      if (!row) return reply.code(409).send({ error: "event type already exists" });
      return reply.code(201).send(row);
    },
  );

  app.get<{ Params: { appId: string } }>(
    "/applications/:appId/event-types",
    async (req) => {
      return query(
        `select id, name, description, created_at from event_types
         where application_id = $1 order by name`,
        [req.params.appId],
      );
    },
  );
}
