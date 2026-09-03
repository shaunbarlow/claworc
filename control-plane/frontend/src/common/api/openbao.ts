import client from "./client";

export interface OpenbaoStatus {
  enabled: boolean;
  configured: boolean;
  status: string;
  error?: string;
  /** Only populated once the workload is reachable ("running"). */
  initialized?: boolean;
  sealed?: boolean;
}

export async function fetchOpenbaoStatus(): Promise<OpenbaoStatus> {
  const { data } = await client.get<OpenbaoStatus>("/openbao/status");
  return data;
}

export interface SharedSecretSet {
  id: number;
  name: string;
}

export async function fetchSharedSecretSets(): Promise<SharedSecretSet[]> {
  const { data } = await client.get<SharedSecretSet[]>("/openbao/shared-sets");
  return data;
}

export async function createSharedSecretSet(name: string): Promise<SharedSecretSet> {
  const { data } = await client.post<SharedSecretSet>("/openbao/shared-sets", { name });
  return data;
}

export async function deleteSharedSecretSet(id: number): Promise<void> {
  await client.delete(`/openbao/shared-sets/${id}`);
}
export interface ResetOpenbaoTokensResult {
  instances: number;
  reminted: number;
  revoked: number;
  failed: number;
}

/**
 * Revokes and re-mints every agent's OpenBao token. Needed after a change
 * that only applies at mint time (e.g. the token TTL ceiling), since tokens
 * are otherwise issued once and kept for their whole lifetime.
 */
export async function resetOpenbaoTokens(): Promise<ResetOpenbaoTokensResult> {
  const { data } = await client.post<ResetOpenbaoTokensResult>("/openbao/reset-tokens");
  return data;
}

/* ---------------------------------------------------------------------------
 * Per-agent secrets (this agent's own OpenBao namespace)
 * ------------------------------------------------------------------------ */

export interface SecretField {
  key: string;
  /** "****" + last 4 chars. Plaintext comes only from revealInstanceSecret. */
  masked: string;
}

export interface SecretEntry {
  /** Path relative to the agent's namespace, e.g. "github/token". */
  path: string;
  version: number;
  updated_at: string;
  fields: SecretField[];
}

export interface InstanceSecrets {
  enabled: boolean;
  /** False while the OpenBao workload is still starting or bootstrapping. */
  ready: boolean;
  /** Full KV v2 path of this agent's namespace, e.g. "secret/agents/<uuid>". */
  base_path: string;
  entries: SecretEntry[];
  /** True when the listing hit the server's entry/depth ceiling. */
  truncated: boolean;
}

export async function fetchInstanceSecrets(instanceId: number): Promise<InstanceSecrets> {
  const { data } = await client.get<InstanceSecrets>(`/instances/${instanceId}/secrets`);
  return data;
}

export interface WriteInstanceSecretPayload {
  path: string;
  key: string;
  value: string;
}

/** Sets one field on one secret, creating the secret if it doesn't exist. */
export async function writeInstanceSecret(
  instanceId: number,
  payload: WriteInstanceSecretPayload,
): Promise<void> {
  await client.put(`/instances/${instanceId}/secrets`, payload);
}

export async function revealInstanceSecret(
  instanceId: number,
  path: string,
  key: string,
): Promise<string> {
  const { data } = await client.get<{ value: string }>(`/instances/${instanceId}/secrets/reveal`, {
    params: { path, key },
  });
  return data.value;
}

/** Deletes one field, or the whole entry when key is omitted. */
export async function deleteInstanceSecret(
  instanceId: number,
  path: string,
  key?: string,
): Promise<void> {
  await client.delete(`/instances/${instanceId}/secrets`, {
    params: key ? { path, key } : { path },
  });
}
