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

export interface ConnectorUpdateImageResponse {
  status: string;
  task_id?: string;
  detail?: string;
}

// updateConnectorImage forces a pull-and-restart of the managed connector
// workload against whatever image reference is currently configured
// (connector_image setting, default the "tip" tag). Needed because that tag
// is mutable upstream -- without this, once the image has been pulled once,
// Claworc had no way to fetch a newer build pushed under the same tag.
export async function updateConnectorImage(): Promise<ConnectorUpdateImageResponse> {
  const { data } = await client.post<ConnectorUpdateImageResponse>("/connector/update-image");
  return data;
}
