import client from "./client";
import type {
  InstallPluginResponse,
  InstancePluginsList,
  PluginActionResult,
  PluginConfigResponse,
} from "@common/types/plugins";

export async function listInstancePlugins(instanceId: number): Promise<InstancePluginsList> {
  const res = await client.get<InstancePluginsList>(`/instances/${instanceId}/plugins`);
  return res.data;
}

export async function installInstancePlugin(
  instanceId: number,
  spec: string,
): Promise<InstallPluginResponse> {
  const res = await client.post<InstallPluginResponse>(`/instances/${instanceId}/plugins`, {
    spec,
  });
  return res.data;
}

export async function enableInstancePlugin(
  instanceId: number,
  pluginId: string,
): Promise<PluginActionResult> {
  const res = await client.post<PluginActionResult>(
    `/instances/${instanceId}/plugins/${pluginId}/enable`,
  );
  return res.data;
}

export async function disableInstancePlugin(
  instanceId: number,
  pluginId: string,
): Promise<PluginActionResult> {
  const res = await client.post<PluginActionResult>(
    `/instances/${instanceId}/plugins/${pluginId}/disable`,
  );
  return res.data;
}

export async function uninstallInstancePlugin(
  instanceId: number,
  pluginId: string,
): Promise<InstallPluginResponse> {
  const res = await client.delete<InstallPluginResponse>(
    `/instances/${instanceId}/plugins/${pluginId}`,
  );
  return res.data;
}

export async function getInstancePluginConfig(
  instanceId: number,
  pluginId: string,
): Promise<PluginConfigResponse> {
  const res = await client.get<PluginConfigResponse>(
    `/instances/${instanceId}/plugins/${pluginId}/config`,
  );
  return res.data;
}

export async function putInstancePluginConfig(
  instanceId: number,
  pluginId: string,
  config: string,
): Promise<PluginActionResult> {
  const res = await client.put<PluginActionResult>(
    `/instances/${instanceId}/plugins/${pluginId}/config`,
    { config },
  );
  return res.data;
}
