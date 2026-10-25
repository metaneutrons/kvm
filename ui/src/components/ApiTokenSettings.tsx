import { useCallback, useEffect, useState } from "react";
import { LuCopy } from "react-icons/lu";

import { Button } from "@components/Button";
import { GridCard } from "@components/Card";
import { NestedSettingsGroup } from "@components/NestedSettingsGroup";
import { SelectMenuBasic } from "@components/SelectMenuBasic";
import { SettingsItem } from "@components/SettingsItem";
import { useCopyToClipboard } from "@components/useCopyToClipBoard";
import api from "@/api";
import notifications from "@/notifications";
import { DEVICE_API } from "@/ui.config";
import { m } from "@localizations/messages.js";

type ApiTokenScope = "full" | "view";

interface ApiTokenState {
  enabled: boolean;
  created_at?: string;
  scope?: ApiTokenScope;
}

// The device returns a new token only once, in the response that creates it.
// It is kept in component state until the page is left and never stored.
export default function ApiTokenSettings() {
  const [tokenState, setTokenState] = useState<ApiTokenState | null>(null);
  const [newToken, setNewToken] = useState<string | null>(null);
  const [isBusy, setIsBusy] = useState(false);
  const [scope, setScope] = useState<ApiTokenScope>("full");
  const { copy } = useCopyToClipboard();

  const loadTokenState = useCallback(async () => {
    const res = await api.GET(`${DEVICE_API}/auth/api-token`);
    if (!res.ok) return;
    setTokenState((await res.json()) as ApiTokenState);
  }, []);

  useEffect(() => {
    loadTokenState();
  }, [loadTokenState]);

  const createToken = async () => {
    if (tokenState?.enabled && !window.confirm(m.access_api_token_confirm_replace())) return;

    setIsBusy(true);
    try {
      const res = await api.POST(`${DEVICE_API}/auth/api-token`, { scope });
      const body = await res.json();
      if (!res.ok) {
        notifications.error(m.access_api_token_failed({ error: body.error || m.unknown_error() }));
        return;
      }
      setNewToken(body.token);
      setTokenState({ enabled: true, created_at: body.created_at, scope: body.scope });
    } finally {
      setIsBusy(false);
    }
  };

  const deleteToken = async () => {
    if (!window.confirm(m.access_api_token_confirm_delete())) return;

    setIsBusy(true);
    try {
      const res = await api.DELETE(`${DEVICE_API}/auth/api-token`);
      if (!res.ok) {
        const body = await res.json();
        notifications.error(m.access_api_token_failed({ error: body.error || m.unknown_error() }));
        return;
      }
      setNewToken(null);
      setTokenState({ enabled: false });
      notifications.success(m.access_api_token_deleted());
    } finally {
      setIsBusy(false);
    }
  };

  if (!tokenState) return null;

  return (
    <>
      <SettingsItem
        title={m.access_api_token_title()}
        description={
          tokenState.enabled && tokenState.created_at
            ? (tokenState.scope === "view"
                ? m.access_api_token_active_view
                : m.access_api_token_active)({
                date: new Date(tokenState.created_at).toLocaleString(),
              })
            : m.access_api_token_description()
        }
      >
        <div className="flex flex-wrap items-center justify-end gap-2">
          <SelectMenuBasic
            className="shrink-0"
            size="SM"
            value={scope}
            onChange={e => setScope(e.target.value as ApiTokenScope)}
            disabled={isBusy}
            options={[
              { value: "full", label: m.access_api_token_scope_full() },
              { value: "view", label: m.access_api_token_scope_view() },
            ]}
          />
          {tokenState.enabled && (
            <Button
              size="SM"
              theme="light"
              text={m.access_api_token_delete()}
              className="text-red-600"
              disabled={isBusy}
              onClick={deleteToken}
            />
          )}
          <Button
            size="SM"
            theme="light"
            text={tokenState.enabled ? m.access_api_token_replace() : m.access_api_token_create()}
            disabled={isBusy}
            onClick={createToken}
          />
        </div>
      </SettingsItem>

      {newToken && (
        <NestedSettingsGroup className="mt-4">
          <SettingsItem
            title={m.access_api_token_new_title()}
            description={m.access_api_token_new_description()}
          />
          <div className="flex items-center">
            <GridCard cardClassName="rounded-r-none min-w-0">
              <div className="flex h-[34px] items-center overflow-x-auto px-3 font-mono text-xs whitespace-nowrap text-black select-all dark:text-white">
                {newToken}
              </div>
            </GridCard>
            <Button
              className="rounded-l-none border-l-slate-800/30 dark:border-slate-300/20"
              size="SM"
              type="button"
              theme="light"
              LeadingIcon={LuCopy}
              onClick={async () => {
                if (await copy(newToken)) {
                  notifications.success(m.access_api_token_copied());
                } else {
                  notifications.error(m.access_api_token_copy_error());
                }
              }}
            />
          </div>
        </NestedSettingsGroup>
      )}
    </>
  );
}
