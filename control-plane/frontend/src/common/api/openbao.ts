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
