"use client";

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { useAuthStore } from "@multica/core/auth";
import {
  feishuWorkspaceSettingOptions,
  larkAccountBindingOptions,
  larkKeys,
} from "@multica/core/lark";
import { agentListOptions, memberListOptions } from "@multica/core/workspace/queries";
import { api } from "@multica/core/api";
import { notificationPreferenceOptions } from "@multica/core/notification-preferences/queries";
import { useUpdateNotificationPreferences } from "@multica/core/notification-preferences/mutations";
import type { NotificationGroupKey, NotificationPreferences } from "@multica/core/types";
import { Switch } from "@multica/ui/components/ui/switch";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { toast } from "sonner";
import { useT } from "../../i18n";
import { BrowserNotificationSetting } from "./browser-notification-setting";
import {
  SettingsCard,
  SettingsRow,
  SettingsSection,
  SettingsTab,
} from "./settings-layout";

// Inbox event groups rendered in the per-event toggle list. `system_notifications`
// is a sibling preference key but lives in its own section below.
const INBOX_GROUP_KEYS = [
  "assignments",
  "status_changes",
  "comments",
  "updates",
  "agent_activity",
] as const;
type InboxGroupKey = (typeof INBOX_GROUP_KEYS)[number];

export function NotificationsTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const { data } = useQuery(notificationPreferenceOptions(wsId));
  const mutation = useUpdateNotificationPreferences();
  const queryClient = useQueryClient();
  const user = useAuthStore((state) => state.user);
  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const { data: feishuSetting } = useQuery(feishuWorkspaceSettingOptions(wsId));
  const { data: accountBinding } = useQuery(larkAccountBindingOptions());
  const currentMember = members.find((member) => member.user_id === user?.id);
  const canManageWorkspace =
    currentMember?.role === "owner" || currentMember?.role === "admin";
  const agentOptions = [
    { value: "__none__", label: t(($) => $.notifications.feishu.not_configured) },
    ...agents
      .filter((agent) => !agent.archived_at)
      .map((agent) => ({ value: agent.id, label: agent.name })),
  ];
  const recipientOptions = [
    { value: "__none__", label: t(($) => $.notifications.feishu.not_configured) },
    ...members.map((member) => ({
      value: member.user_id,
      label: member.name || member.email,
    })),
  ];

  const preferences = data?.preferences ?? {};

  const handleToggle = (key: NotificationGroupKey, enabled: boolean) => {
    const updated: NotificationPreferences = {
      ...preferences,
      [key]: enabled ? "all" : "muted",
    };
    // Remove keys set to "all" (default) to keep the object clean
    if (enabled) {
      delete updated[key];
    }
    mutation.mutate(updated, {
      onSuccess: () =>
        toast.success(t(($) => $.auto_save.toast_saved), {
          id: "settings-auto-save",
        }),
      onError: (err) =>
        toast.error(
          err instanceof Error && err.message
            ? err.message
            : t(($) => $.notifications.toast_failed),
        ),
    });
  };

  const systemEnabled = preferences.system_notifications !== "muted";

  const updateFeishu = async (patch: Parameters<typeof api.updateFeishuWorkspaceSetting>[1]) => {
    try {
      await api.updateFeishuWorkspaceSetting(wsId, patch);
      await queryClient.invalidateQueries({ queryKey: larkKeys.workspaceSetting(wsId) });
      toast.success(t(($) => $.notifications.feishu.saved), {
        id: "settings-auto-save",
      });
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t(($) => $.notifications.feishu.save_failed),
      );
    }
  };

  return (
    <SettingsTab title={t(($) => $.page.tabs.notifications)}>
      <SettingsSection
        title={t(($) => $.notifications.title)}
        description={t(($) => $.notifications.description)}
      >
        <SettingsCard>
            {INBOX_GROUP_KEYS.map((key: InboxGroupKey) => {
              const enabled = preferences[key] !== "muted";
              return (
                <SettingsRow
                  key={key}
                  label={t(($) => $.notifications.groups[key].label)}
                  description={t(($) => $.notifications.groups[key].description)}
                >
                  <Switch
                    checked={enabled}
                    aria-label={t(($) => $.notifications.groups[key].label)}
                    onCheckedChange={(checked) => handleToggle(key, checked)}
                  />
                </SettingsRow>
              );
            })}
        </SettingsCard>
      </SettingsSection>

      <SettingsSection
        title={t(($) => $.notifications.system.title)}
        description={t(($) => $.notifications.system.description)}
      >
        <SettingsCard>
          <SettingsRow
            label={t(($) => $.notifications.system.label)}
            description={t(($) => $.notifications.system.hint)}
          >
              <Switch
                checked={systemEnabled}
                aria-label={t(($) => $.notifications.system.label)}
                onCheckedChange={(checked) => handleToggle("system_notifications", checked)}
              />
          </SettingsRow>
        </SettingsCard>

        {/* Web-only: the browser permission banners require. Renders nothing on
            desktop (OS-native delivery) or where the Notification API is absent. */}
        <BrowserNotificationSetting />
      </SettingsSection>

      <SettingsSection
        title={t(($) => $.notifications.feishu.title)}
        description={t(($) => $.notifications.feishu.description)}
      >
        <SettingsCard>
          <SettingsRow
            label={t(($) => $.notifications.feishu.default_agent)}
            description={t(($) => $.notifications.feishu.default_agent_description)}
          >
            <Select
              items={agentOptions}
              value={feishuSetting?.default_agent_id ?? "__none__"}
              disabled={!canManageWorkspace}
              onValueChange={(value) => {
                if (!value) return;
                void updateFeishu({ default_agent_id: value === "__none__" ? null : value });
              }}
            >
              <SelectTrigger size="sm" className="w-48">
                <SelectValue>
                  {agentOptions.find(
                    (option) =>
                      option.value === (feishuSetting?.default_agent_id ?? "__none__"),
                  )?.label}
                </SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="__none__">
                  {t(($) => $.notifications.feishu.not_configured)}
                </SelectItem>
                {agents
                  .filter((agent) => !agent.archived_at)
                  .map((agent) => (
                    <SelectItem key={agent.id} value={agent.id}>
                      {agent.name}
                    </SelectItem>
                  ))}
              </SelectContent>
            </Select>
          </SettingsRow>

          <SettingsRow
            label={t(($) => $.notifications.feishu.recipient)}
            description={
              accountBinding?.bound
                ? t(($) => $.notifications.feishu.recipient_bound_description)
                : t(($) => $.notifications.feishu.recipient_unbound_description)
            }
          >
            <Select
              items={recipientOptions}
              value={feishuSetting?.notification_recipient_user_id ?? "__none__"}
              disabled={!canManageWorkspace}
              onValueChange={(value) => {
                if (!value) return;
                void updateFeishu({
                  notification_recipient_user_id: value === "__none__" ? null : value,
                  ...(value === "__none__" ? { notification_enabled: false } : {}),
                });
              }}
            >
              <SelectTrigger size="sm" className="w-48">
                <SelectValue>
                  {recipientOptions.find(
                    (option) =>
                      option.value ===
                      (feishuSetting?.notification_recipient_user_id ?? "__none__"),
                  )?.label}
                </SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="__none__">
                  {t(($) => $.notifications.feishu.not_configured)}
                </SelectItem>
                {members.map((member) => (
                  <SelectItem key={member.user_id} value={member.user_id}>
                    {member.name || member.email}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </SettingsRow>

          <SettingsRow
            label={t(($) => $.notifications.feishu.issue_status)}
            description={t(($) => $.notifications.feishu.issue_status_description)}
          >
            <Switch
              checked={feishuSetting?.notification_enabled ?? false}
              disabled={
                !canManageWorkspace ||
                !feishuSetting?.notification_recipient_user_id
              }
              onCheckedChange={(checked) =>
                void updateFeishu({
                  notification_enabled: checked,
                  notification_events: ["issue.done", "issue.blocked"],
                })
              }
            />
          </SettingsRow>
        </SettingsCard>
      </SettingsSection>
    </SettingsTab>
  );
}
