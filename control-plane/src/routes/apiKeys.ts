import { createHash, randomBytes } from "node:crypto";
import type { FastifyInstance } from "fastify";
import { query, queryOne } from "../db.js";

// Key format: relay_sk_<40 hex>. The first 16 chars are the indexed lookup
// prefix; only the sha256 of the full key is stored. Matches the ingest
// service's authentication scheme (go/cmd/ingest).
const KEY_PREFIX_LEN = 16;

export async function apiKeyRoutes(app: FastifyInstance) {
  app.post<{ Params: { appId: string } }>(
    "/applications/:appId/api-keys",
    async (req, reply) => {
      const raw = "relay_sk_" + randomBytes(20).toString("hex");
      const row = await queryOne(
        `insert into api_keys (application_id, prefix, key_hash)
         values ($1, $2, $3)
         returning id, application_id, prefix, created_at`,
        [
          req.params.appId,
          raw.slice(0, KEY_PREFIX_LEN),
          createHash("sha256").update(raw).digest("hex"),
        ],
      );
      // The raw key is shown exactly once — it cannot be recovered later.
      return reply.code(201).send({ ...row, key: raw });
    },
  );

  app.get<{ Params: { appId: string } }>(
    "/applications/:appId/api-keys",
    async (req) => {
      return query(
        `select id, prefix, created_at, revoked_at from api_keys
         where application_id = $1 order by created_at`,
        [req.params.appId],
      );
    },
  );

  app.delete<{ Params: { id: string } }>("/api-keys/:id", async (req, reply) => {
    const row = await queryOne(
      `update api_keys set revoked_at = now()
       where id = $1 and revoked_at is null
       returning id`,
      [req.params.id],
    );
    if (!row) return reply.code(404).send({ error: "key not found or already revoked" });
    return reply.code(204).send();
  });
}
