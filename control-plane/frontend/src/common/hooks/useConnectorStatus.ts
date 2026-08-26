import { useQuery } from "@tanstack/react-query";
import { fetchConnectorStatus } from "@common/api/connector";

export function useConnectorStatus(enabled: boolean) {
  return useQuery({
    queryKey: ["connector-status"],
    queryFn: fetchConnectorStatus,
    enabled,
    refetchInterval: enabled ? 10_000 : false,
  });
}
