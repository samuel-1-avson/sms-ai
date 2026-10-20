// Typed wrapper for the Kotlin SmsGateway module. The gateway runs in a
// native foreground service; these calls configure it and read its state.
import { requireNativeModule } from "expo";

export interface GatewayStatus {
  configured: boolean;
  serverUrl: string | null;
  enabled: boolean;
  running: boolean;
  paused: boolean;
  subscriptionId: number;
  sendIntervalMs: number;
  deviceId: string;
  lastError: string | null;
  lastContactAt: number;
  inboxPending: number;
  outboxPending: number;
  sending: number;
  statusPending: number;
  sentToday: number;
  receivedToday: number;
}

export interface GatewayPermissions {
  receiveSms: boolean;
  sendSms: boolean;
  readPhoneState: boolean;
  notifications: boolean;
  ignoringBatteryOptimizations: boolean;
}

export interface Sim {
  subscriptionId: number;
  slot: number;
  label: string;
  carrier: string;
}

export interface GatewayEvent {
  at: number;
  kind: "receive" | "send" | "error" | "service";
  text: string;
}

interface SmsGatewayNative {
  getStatus(): GatewayStatus;
  getPermissions(): GatewayPermissions;
  configure(serverUrl: string, secret: string): void;
  unpair(): void;
  start(): void;
  stop(): void;
  setPaused(paused: boolean): void;
  setSubscriptionId(id: number): void;
  setSendIntervalMs(ms: number): void;
  listSims(): Sim[];
  recentEvents(limit: number): GatewayEvent[];
  testConnection(serverUrl: string, secret: string): Promise<{ ok: boolean; serverTime?: string; error: string | null }>;
  requestIgnoreBatteryOptimizations(): void;
  openAppSettings(): void;
}

export const SmsGateway = requireNativeModule<SmsGatewayNative>("SmsGateway");
