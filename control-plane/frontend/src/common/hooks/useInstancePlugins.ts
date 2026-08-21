import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  disableInstancePlugin,
  enableInstancePlugin,
  getInstancePluginConfig,
  installInstancePlugin,
  listInstancePlugins,
  putInstancePluginConfig,
  uninstallInstancePlugin,
} from "@common/api/plugins";
import { errorToast, successToast } from "@common/utils/toast";

export function useInstancePlugins(instanceId: number | undefined) {
  return useQuery({
    queryKey: ["instance-plugins", instanceId],
    queryFn: () => listInstancePlugins(instanceId!),
    enabled: !!instanceId,
    // Same contract as the Discord/Slack plugin readback: the first load can
    // come back "checking" while the probe runs in the background.
    refetchInterval: (query) => (query.state.data?.state === "checking" ? 5000 : false),
  });
}

export function useInstallInstancePlugin(instanceId: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (spec: string) => installInstancePlugin(instanceId, spec),
    onSuccess: () => {
      successToast("Install started", "Check back shortly for the result.");
      qc.invalidateQueries({ queryKey: ["instance-plugins", instanceId] });
    },
    onError: (error) => errorToast("Failed to start plugin install", error),
  });
}

export function useEnableInstancePlugin(instanceId: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (pluginId: string) => enableInstancePlugin(instanceId, pluginId),
    onSuccess: (res) => {
      if (!res.ok) {
        errorToast("Failed to enable plugin", new Error(res.error ?? "unknown error"));
        return;
      }
      successToast("Plugin enabled");
      qc.invalidateQueries({ queryKey: ["instance-plugins", instanceId] });
    },
    onError: (error) => errorToast("Failed to enable plugin", error),
  });
}

export function useDisableInstancePlugin(instanceId: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (pluginId: string) => disableInstancePlugin(instanceId, pluginId),
    onSuccess: (res) => {
      if (!res.ok) {
        errorToast("Failed to disable plugin", new Error(res.error ?? "unknown error"));
        return;
      }
      successToast("Plugin disabled");
      qc.invalidateQueries({ queryKey: ["instance-plugins", instanceId] });
    },
    onError: (error) => errorToast("Failed to disable plugin", error),
  });
}

export function useUninstallInstancePlugin(instanceId: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (pluginId: string) => uninstallInstancePlugin(instanceId, pluginId),
    onSuccess: () => {
      successToast("Uninstall started", "Check back shortly for the result.");
      qc.invalidateQueries({ queryKey: ["instance-plugins", instanceId] });
    },
    onError: (error) => errorToast("Failed to start plugin uninstall", error),
  });
}

export function useInstancePluginConfig(instanceId: number | null, pluginId: string | null) {
  return useQuery({
    queryKey: ["instance-plugin-config", instanceId, pluginId],
    queryFn: () => getInstancePluginConfig(instanceId as number, pluginId as string),
    enabled: !!instanceId && !!pluginId,
  });
}

export function useSaveInstancePluginConfig(instanceId: number, pluginId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (config: string) => putInstancePluginConfig(instanceId, pluginId, config),
    onSuccess: (res) => {
      if (!res.ok) {
        errorToast("Failed to save plugin config", new Error(res.error ?? "unknown error"));
        return;
      }
      successToast("Plugin config saved");
      qc.invalidateQueries({ queryKey: ["instance-plugin-config", instanceId, pluginId] });
      qc.invalidateQueries({ queryKey: ["instance-plugins", instanceId] });
    },
    onError: (error) => errorToast("Failed to save plugin config", error),
  });
}
