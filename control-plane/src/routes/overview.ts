import type { FastifyInstance } from "fastify";
import { query, queryOne } from "../db.js";

// Aggregates powering the dashboard's stat tiles. One round trip each, all
// bounded to the last 24h so the queries stay cheap regardless of table size.
export async function overviewRoutes(app: FastifyInstance) {
  app.get("/overview", async () => {
    const totals = await queryOne<{
      events_24h: string;
      applications: string;
      endpoints: string;
    }>(
      `select
         (select count(*) from events where received_at > now() - interval '24 hours') as events_24h,
         (select count(*) from applications) as applications,
         (select count(*) from endpoints where not disabled) as endpoints`,
    );
    const byStatus = await query<{ status: string; count: string }>(
      `select status, count(*) as count
       from deliveries
       where created_at > now() - interval '24 hours'
       group by status`,
    );
    const deliveries: Record<string, number> = {
      pending: 0,
      succeeded: 0,
      failed: 0,
      dead: 0,
    };
    for (const row of byStatus) deliveries[row.status] = Number(row.count);
    return {
      events_24h: Number(totals?.events_24h ?? 0),
      applications: Number(totals?.applications ?? 0),
      endpoints: Number(totals?.endpoints ?? 0),
      deliveries,
    };
  });
}
