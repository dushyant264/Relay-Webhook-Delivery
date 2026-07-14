import type { FastifyInstance } from "fastify";
import { z } from "zod";
import { query, queryOne } from "../db.js";

const listQuery = z.object({
  limit: z.coerce.number().int().min(1).max(200).default(50),
  status: z.enum(["pending", "succeeded", "failed", "dead"]).optional(),
});

// Read-side of the audit trail: what happened to my event / my endpoint?
export async function deliveryRoutes(app: FastifyInstance) {
  // Global recent-deliveries feed (dashboard).
  app.get<{ Querystring: unknown }>("/deliveries", async (req) => {
    const q = listQuery.parse(req.query);
    return query(
      `select d.id, d.event_id, ev.event_type, d.endpoint_id, ep.url,
              d.status, d.attempt_count, d.created_at, d.updated_at
       from deliveries d
       join events ev on ev.id = d.event_id
       join endpoints ep on ep.id = d.endpoint_id
       where ($1::text is null or d.status = $1)
       order by d.created_at desc limit $2`,
      [q.status ?? null, q.limit],
    );
  });

  app.get<{ Params: { eventId: string } }>(
    "/events/:eventId/deliveries",
    async (req) => {
      return query(
        `select d.id, d.endpoint_id, ep.url, d.status, d.attempt_count, d.created_at, d.updated_at
         from deliveries d join endpoints ep on ep.id = d.endpoint_id
         where d.event_id = $1 order by d.created_at`,
        [req.params.eventId],
      );
    },
  );

  app.get<{ Params: { endpointId: string }; Querystring: unknown }>(
    "/endpoints/:endpointId/deliveries",
    async (req) => {
      const q = listQuery.parse(req.query);
      return query(
        `select d.id, d.event_id, ev.event_type, d.status, d.attempt_count, d.created_at, d.updated_at
         from deliveries d join events ev on ev.id = d.event_id
         where d.endpoint_id = $1 and ($2::text is null or d.status = $2)
         order by d.created_at desc limit $3`,
        [req.params.endpointId, q.status ?? null, q.limit],
      );
    },
  );

  // Full drill-down: the delivery plus every attempt (status codes, errors,
  // latencies) — the "why did my webhook fail" debugging view.
  app.get<{ Params: { id: string } }>("/deliveries/:id", async (req, reply) => {
    const delivery = await queryOne(
      `select d.id, d.event_id, ev.event_type, ev.payload, d.endpoint_id, ep.url,
              d.status, d.attempt_count, d.created_at, d.updated_at
       from deliveries d
       join events ev on ev.id = d.event_id
       join endpoints ep on ep.id = d.endpoint_id
       where d.id = $1`,
      [req.params.id],
    );
    if (!delivery) return reply.code(404).send({ error: "delivery not found" });
    const attempts = await query(
      `select attempt_no, status_code, success, error, duration_ms, created_at
       from delivery_attempts where delivery_id = $1 order by attempt_no`,
      [req.params.id],
    );
    return { ...delivery, attempts };
  });
}
