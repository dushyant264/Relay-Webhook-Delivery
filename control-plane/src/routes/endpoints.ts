import { randomBytes } from "node:crypto";
import type { FastifyInstance } from "fastify";
import { z } from "zod";
import { pool, query, queryOne } from "../db.js";

const createEndpoint = z.object({
  url: z.string().url(),
  description: z.string().max(500).optional(),
  rate_limit_per_sec: z.number().int().min(1).max(1000).default(5),
  event_types: z.array(z.string().min(1)).min(1),
  // Optional import of an existing HMAC secret (e.g. migrating from another
  // provider, or pointing several endpoints at one receiver); generated when omitted.
  secret: z.string().min(16).max(120).optional(),
});

const patchEndpoint = z.object({
  disabled: z.boolean().optional(),
  rate_limit_per_sec: z.number().int().min(1).max(1000).optional(),
  event_types: z.array(z.string().min(1)).min(1).optional(),
});

export async function endpointRoutes(app: FastifyInstance) {
  // Creates the endpoint and its subscriptions atomically. The HMAC secret is
  // generated server-side and returned once here; subscribers configure it on
  // their end to verify Relay-Signature headers.
  app.post<{ Params: { appId: string } }>(
    "/applications/:appId/endpoints",
    async (req, reply) => {
      const body = createEndpoint.parse(req.body);
      const secret = body.secret ?? "whsec_" + randomBytes(16).toString("hex");

      const client = await pool.connect();
      try {
        await client.query("begin");
        const {
          rows: [endpoint],
        } = await client.query(
          `insert into endpoints (application_id, url, secret, description, rate_limit_per_sec)
           values ($1, $2, $3, $4, $5)
           returning id, application_id, url, description, rate_limit_per_sec, disabled, created_at`,
          [req.params.appId, body.url, secret, body.description ?? null, body.rate_limit_per_sec],
        );
        for (const et of body.event_types) {
          await client.query(
            `insert into endpoint_subscriptions (endpoint_id, event_type) values ($1, $2)`,
            [endpoint.id, et],
          );
        }
        await client.query("commit");
        return reply.code(201).send({ ...endpoint, secret, event_types: body.event_types });
      } catch (err) {
        await client.query("rollback");
        throw err;
      } finally {
        client.release();
      }
    },
  );

  app.get<{ Params: { appId: string } }>(
    "/applications/:appId/endpoints",
    async (req) => {
      return query(
        `select e.id, e.url, e.description, e.rate_limit_per_sec, e.disabled, e.created_at,
                coalesce(array_agg(es.event_type order by es.event_type)
                         filter (where es.event_type is not null), '{}') as event_types
         from endpoints e
         left join endpoint_subscriptions es on es.endpoint_id = e.id
         where e.application_id = $1
         group by e.id
         order by e.created_at`,
        [req.params.appId],
      );
    },
  );

  app.patch<{ Params: { id: string } }>("/endpoints/:id", async (req, reply) => {
    const body = patchEndpoint.parse(req.body);
    const client = await pool.connect();
    try {
      await client.query("begin");
      const {
        rows: [endpoint],
      } = await client.query(
        `update endpoints
         set disabled = coalesce($2, disabled),
             rate_limit_per_sec = coalesce($3, rate_limit_per_sec)
         where id = $1
         returning id, url, description, rate_limit_per_sec, disabled`,
        [req.params.id, body.disabled ?? null, body.rate_limit_per_sec ?? null],
      );
      if (!endpoint) {
        await client.query("rollback");
        return reply.code(404).send({ error: "endpoint not found" });
      }
      if (body.event_types) {
        await client.query(`delete from endpoint_subscriptions where endpoint_id = $1`, [
          endpoint.id,
        ]);
        for (const et of body.event_types) {
          await client.query(
            `insert into endpoint_subscriptions (endpoint_id, event_type) values ($1, $2)`,
            [endpoint.id, et],
          );
        }
      }
      await client.query("commit");
      return endpoint;
    } catch (err) {
      await client.query("rollback");
      throw err;
    } finally {
      client.release();
    }
  });

  app.delete<{ Params: { id: string } }>("/endpoints/:id", async (req, reply) => {
    const row = await queryOne(`delete from endpoints where id = $1 returning id`, [
      req.params.id,
    ]);
    if (!row) return reply.code(404).send({ error: "endpoint not found" });
    return reply.code(204).send();
  });
}
