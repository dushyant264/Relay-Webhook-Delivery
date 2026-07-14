import type { FastifyInstance } from "fastify";
import { z } from "zod";
import { query, queryOne } from "../db.js";

const createTenant = z.object({ name: z.string().min(1).max(120) });

export async function tenantRoutes(app: FastifyInstance) {
  app.post("/tenants", async (req, reply) => {
    const body = createTenant.parse(req.body);
    const row = await queryOne(
      `insert into tenants (name) values ($1)
       on conflict (name) do nothing
       returning id, name, created_at`,
      [body.name],
    );
    if (!row) return reply.code(409).send({ error: "tenant name already exists" });
    return reply.code(201).send(row);
  });

  app.get("/tenants", async () => {
    return query(`select id, name, created_at from tenants order by created_at`);
  });
}
