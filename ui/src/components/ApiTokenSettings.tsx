import { useCallback, useEffect, useState } from "react";
import { LuCopy } from "react-icons/lu";

import { Button } from "@components/Button";
import { GridCard } from "@components/Card";
import { Checkbox, CheckboxWithLabel } from "@components/Checkbox";
import { InputFieldWithLabel } from "@components/InputField";
import { NestedSettingsGroup } from "@components/NestedSettingsGroup";
import { SelectMenuBasic } from "@components/SelectMenuBasic";
import { SettingsItem } from "@components/SettingsItem";
import { useCopyToClipboard } from "@components/useCopyToClipBoard";
import api from "@/api";
import notifications from "@/notifications";
import { DEVICE_API } from "@/ui.config";
import { m } from "@localizations/messages.js";

type ApiPermission = "view" | "input" | "media" | "power";
type OptionalPermission = Exclude<ApiPermission, "view">;

interface ApiTokenInfo {
  id: string;
  name: string;
  permissions: ApiPermission[];
  created_at: string;
  expires_at?: string;
  expired?: boolean;
  last_used_at?: string;
  last_used_ip?: string;
}

interface ApiTokenList {
  tokens: ApiTokenInfo[];
  limit: number;
  mcp_enabled: boolean;
}

const EXPIRY_DAYS = [0, 7, 30, 90, 365];

function permissionLabel(permission: ApiPermission) {
  switch (permission) {
    case "view":
      return m.access_api_token_perm_view();
    case "input":
      return m.access_api_token_perm_input();
    case "media":
      return m.access_api_token_perm_media();
    case "power":
      return m.access_api_token_perm_power();
  }
}

function permissionDescription(permission: ApiPermission) {
  switch (permission) {
    case "view":
      return m.access_api_token_perm_view_description();
    case "input":
      return m.access_api_token_perm_input_description();
    case "media":
      return m.access_api_token_perm_media_description();
    case "power":
      return m.access_api_token_perm_power_description();
  }
}

function formatDate(value: string) {
  return new Date(value).toLocaleString();
}

function TokenRow({
  token,
  disabled,
  onDelete,
}: {
  token: ApiTokenInfo;
  disabled: boolean;
  onDelete: (token: ApiTokenInfo) => void;
}) {
  const details = [
    m.access_api_token_created({ date: formatDate(token.created_at) }),
    token.last_used_at
      ? m.access_api_token_last_used({
          date: formatDate(token.last_used_at),
          ip: token.last_used_ip ?? "",
        })
      : m.access_api_token_never_used(),
  ];
  if (token.expires_at) {
    details.push(
      (token.expired ? m.access_api_token_expired : m.access_api_token_expires)({
        date: formatDate(token.expires_at),
      }),
    );
  }

  return (
    <div className="flex items-start justify-between gap-x-4 py-3">
      <div className="min-w-0 space-y-1">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
          <span className="font-semibold break-all text-black dark:text-white">{token.name}</span>
          {token.permissions.map(permission => (
            <span
              key={permission}
              className="rounded-full bg-slate-200 px-2 py-0.5 text-[11px] leading-none font-medium text-slate-700 dark:bg-slate-700 dark:text-slate-200"
            >
              {permissionLabel(permission)}
            </span>
          ))}
        </div>
        <p
          className={
            token.expired
              ? "text-xs text-red-600 dark:text-red-400"
              : "text-xs text-slate-700 dark:text-slate-300"
          }
        >
          {details.join(" · ")}
        </p>
      </div>
      <Button
        size="SM"
        theme="light"
        text={m.access_api_token_delete()}
        className="shrink-0 text-red-600"
        disabled={disabled}
        onClick={() => onDelete(token)}
      />
    </div>
  );
}

// The device returns a new token only once, in the response that creates it.
// It is kept in component state until the page is left and never stored.
export default function ApiTokenSettings({ passwordMode }: { passwordMode: boolean }) {
  const [list, setList] = useState<ApiTokenList | null>(null);
  const [newToken, setNewToken] = useState<string | null>(null);
  const [isBusy, setIsBusy] = useState(false);
  const [name, setName] = useState("");
  const [permissions, setPermissions] = useState<OptionalPermission[]>([]);
  const [days, setDays] = useState("0");
  const { copy } = useCopyToClipboard();

  const loadTokens = useCallback(async () => {
    const res = await api.GET(`${DEVICE_API}/auth/api-tokens`);
    if (!res.ok) return;
    setList((await res.json()) as ApiTokenList);
  }, []);

  useEffect(() => {
    loadTokens();
  }, [loadTokens]);

  const fail = async (res: Response) => {
    const body = await res.json().catch(() => ({}));
    notifications.error(m.access_api_token_failed({ error: body.error || m.unknown_error() }));
  };

  const createToken = async () => {
    setIsBusy(true);
    try {
      const res = await api.POST(`${DEVICE_API}/auth/api-tokens`, {
        name: name.trim(),
        permissions,
        expires_in_days: Number(days),
      });
      if (!res.ok) {
        await fail(res);
        return;
      }
      const body = await res.json();
      setNewToken(body.token);
      setName("");
      await loadTokens();
    } finally {
      setIsBusy(false);
    }
  };

  const deleteToken = async (token: ApiTokenInfo) => {
    if (!window.confirm(m.access_api_token_confirm_delete({ name: token.name }))) return;

    setIsBusy(true);
    try {
      const res = await api.DELETE(`${DEVICE_API}/auth/api-tokens/${token.id}`);
      if (!res.ok) {
        await fail(res);
        return;
      }
      setNewToken(null);
      await loadTokens();
      notifications.success(m.access_api_token_deleted());
    } finally {
      setIsBusy(false);
    }
  };

  const setMcpEnabled = async (enabled: boolean) => {
    setIsBusy(true);
    try {
      const res = await api.PUT(`${DEVICE_API}/auth/mcp`, { enabled });
      if (!res.ok) {
        await fail(res);
        return;
      }
      await loadTokens();
    } finally {
      setIsBusy(false);
    }
  };

  const togglePermission = (permission: OptionalPermission, checked: boolean) =>
    setPermissions(current =>
      checked ? [...current, permission] : current.filter(p => p !== permission),
    );

  if (!list) return null;

  const atLimit = list.tokens.length >= list.limit;

  return (
    <>
      <SettingsItem title={m.access_mcp_title()} description={m.access_mcp_description()}>
        <Checkbox
          checked={list.mcp_enabled}
          disabled={isBusy}
          onChange={e => setMcpEnabled(e.target.checked)}
        />
      </SettingsItem>

      {passwordMode && (
        <>
          <SettingsItem
            title={m.access_api_token_title()}
            description={m.access_api_token_description()}
          />

          {list.tokens.length > 0 && (
            <NestedSettingsGroup>
              <div className="divide-y divide-slate-800/10 dark:divide-slate-300/20">
                {list.tokens.map(token => (
                  <TokenRow key={token.id} token={token} disabled={isBusy} onDelete={deleteToken} />
                ))}
              </div>
            </NestedSettingsGroup>
          )}

          {newToken && (
            <NestedSettingsGroup>
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

          {!atLimit && (
            <NestedSettingsGroup>
              <form
                className="space-y-4"
                onSubmit={e => {
                  e.preventDefault();
                  createToken();
                }}
              >
                <InputFieldWithLabel
                  label={m.access_api_token_name_label()}
                  placeholder={m.access_api_token_name_placeholder()}
                  maxLength={40}
                  value={name}
                  disabled={isBusy}
                  onChange={e => setName(e.target.value)}
                />
                <div className="space-y-2">
                  <CheckboxWithLabel
                    label={permissionLabel("view")}
                    description={permissionDescription("view")}
                    checked
                    disabled
                    readOnly
                  />
                  {(["input", "media", "power"] as const).map(permission => (
                    <CheckboxWithLabel
                      key={permission}
                      label={permissionLabel(permission)}
                      description={permissionDescription(permission)}
                      checked={permissions.includes(permission)}
                      disabled={isBusy}
                      onChange={e => togglePermission(permission, e.target.checked)}
                    />
                  ))}
                </div>
                <div className="flex flex-wrap items-end justify-between gap-2">
                  <SelectMenuBasic
                    size="SM"
                    label={m.access_api_token_expiry_label()}
                    value={days}
                    disabled={isBusy}
                    onChange={e => setDays(e.target.value)}
                    options={EXPIRY_DAYS.map(d => ({
                      value: String(d),
                      label:
                        d === 0
                          ? m.access_api_token_expiry_never()
                          : m.access_api_token_expiry_days({ days: d }),
                    }))}
                  />
                  <Button
                    size="SM"
                    theme="light"
                    type="submit"
                    text={m.access_api_token_create()}
                    disabled={isBusy || name.trim() === ""}
                  />
                </div>
              </form>
            </NestedSettingsGroup>
          )}
        </>
      )}
    </>
  );
}
