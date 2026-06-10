import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, SessionExpiredError } from "./client";

// useMe loads the current user. A 401 (SessionExpiredError) is NOT retried —
// it is the normal "not logged in" signal, consumed by the route guard.
export function useMe() {
  return useQuery({
    queryKey: ["me"],
    queryFn: api.me,
    retry: (failureCount, error) => {
      if (error instanceof SessionExpiredError) return false;
      return failureCount < 2;
    },
  });
}

export function useLogout() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: api.logout,
    onSuccess: () => {
      qc.clear();
    },
  });
}
