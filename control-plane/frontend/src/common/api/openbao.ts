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
