import client from "./client";

export interface ConnectorStatus {
  enabled: boolean;
  configured: boolean;
  status: string;
  error?: string;
}

export async function fetchConnectorStatus(): Promise<ConnectorStatus> {
  const { data } = await client.get<ConnectorStatus>("/connector/status");
  return data;
}
