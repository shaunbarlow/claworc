import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  createSharedSecretSet,
  deleteInstanceSecret,
  deleteSharedSecretSet,
  fetchInstanceSecrets,
  fetchOpenbaoStatus,
  fetchSharedSecretSets,
  resetOpenbaoTokens,
  writeInstanceSecret,
} from "@common/api/openbao";
import type { WriteInstanceSecretPayload } from "@common/api/openbao";
import { successToast, errorToast } from "@common/utils/toast";

export function useOpenbaoStatus(enabled: boolean) {
  return useQuery({
    queryKey: ["openbao-status"],
    queryFn: fetchOpenbaoStatus,
    enabled,
    refetchInterval: enabled ? 10_000 : false,
  });
}

export function useSharedSecretSets() {
  return useQuery({
    queryKey: ["openbao-shared-sets"],
    queryFn: fetchSharedSecretSets,
  });
}

export function useCreateSharedSecretSet() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => createSharedSecretSet(name),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["openbao-shared-sets"] });
      qc.invalidateQueries({ queryKey: ["settings"] });
      successToast("Shared secret set created");
    },
    onError: (err) => errorToast("Failed to create shared secret set", err),
  });
}

export function useDeleteSharedSecretSet() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => deleteSharedSecretSet(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["openbao-shared-sets"] });
      qc.invalidateQueries({ queryKey: ["settings"] });
      successToast("Shared secret set deleted");
    },
    onError: (err) => errorToast("Failed to delete shared secret set", err),
  });
}

export function useResetOpenbaoTokens() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: resetOpenbaoTokens,
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ["openbao-status"] });
      qc.invalidateQueries({ queryKey: ["instances"] });
      const failed = res.failed > 0 ? `, ${res.failed} failed` : "";
      successToast(`Re-minted ${res.reminted} of ${res.instances} agent tokens${failed}`);
    },
    onError: (err) => errorToast("Failed to reset agent tokens", err),
  });
}

/* ---------------------------------------------------------------------------
 * Per-agent secrets
 * ------------------------------------------------------------------------ */

export function useInstanceSecrets(instanceId: number, enabled: boolean) {
  return useQuery({
    queryKey: ["instance-secrets", instanceId],
    queryFn: () => fetchInstanceSecrets(instanceId),
    enabled,
    // No polling: the list is only changed from this panel or by the agent
    // itself, and every value in it -- masked or not -- is secret material,
    // so an idle tab should not keep asking for it.
    refetchOnWindowFocus: false,
  });
}

export function useWriteInstanceSecret(instanceId: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (payload: WriteInstanceSecretPayload) => writeInstanceSecret(instanceId, payload),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["instance-secrets", instanceId] });
      successToast("Secret saved");
    },
    onError: (err) => errorToast("Failed to save secret", err),
  });
}

export function useDeleteInstanceSecret(instanceId: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ path, key }: { path: string; key?: string }) =>
      deleteInstanceSecret(instanceId, path, key),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["instance-secrets", instanceId] });
      successToast("Secret deleted");
    },
    onError: (err) => errorToast("Failed to delete secret", err),
  });
}
