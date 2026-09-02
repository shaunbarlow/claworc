import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  createSharedSecretSet,
  deleteSharedSecretSet,
  fetchOpenbaoStatus,
  fetchSharedSecretSets,
  resetOpenbaoTokens,
} from "@common/api/openbao";
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
