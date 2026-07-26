import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

/** Query key namespace for everything Lark-installation-related. Realtime
 * sync invalidates `installations(wsId)` on `lark_installation:*` events
 * so the Settings panel updates without a refetch. */
export const larkKeys = {
  all: (wsId: string) => ["lark", wsId] as const,
  installations: (wsId: string) => [...larkKeys.all(wsId), "installations"] as const,
  workspaceSetting: (wsId: string) => [...larkKeys.all(wsId), "workspace-setting"] as const,
  accountBinding: () => ["lark", "account-binding"] as const,
  instance: () => ["lark", "instance"] as const,
};

export const larkInstallationsOptions = (wsId: string) =>
  queryOptions({
    queryKey: larkKeys.installations(wsId),
    queryFn: () => api.listLarkInstallations(wsId),
    enabled: !!wsId,
  });

export const feishuWorkspaceSettingOptions = (wsId: string) =>
  queryOptions({
    queryKey: larkKeys.workspaceSetting(wsId),
    queryFn: () => api.getFeishuWorkspaceSetting(wsId),
    enabled: !!wsId,
  });

export const larkAccountBindingOptions = () =>
  queryOptions({
    queryKey: larkKeys.accountBinding(),
    queryFn: () => api.getLarkAccountBinding(),
  });

export const instanceBootstrapOptions = () =>
  queryOptions({
    queryKey: larkKeys.instance(),
    queryFn: () => api.getInstanceBootstrap(),
  });
