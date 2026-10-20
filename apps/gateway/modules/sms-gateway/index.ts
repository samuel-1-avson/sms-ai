// Typed wrapper for the Kotlin SmsGateway module. The SMS AI runs in a
// native foreground service; these calls configure it and read its state.
import { requireNativeModule } from "expo";

export interface GatewayStatus {
  /** An API key is saved. */
  configured: boolean;
  enabled: boolean;
  running: boolean;
  paused: boolean;
  lastError: string | null;
  lastAnswerAt: number;
  /** "closed", or "open: <reason>" while AI calls are paused after errors. */
  aiBreaker: string;
  /** Received texts not yet answered. */
  waiting: number;
  outboxPending: number;
  sending: number;
  sentToday: number;
  failedToday: number;
  receivedToday: number;
  answeredToday: number;
  smsPartsToday: number;
  aiCostToday: number;
  aiCallsToday: number;
  aiFailuresToday: number;
}

export interface GatewaySettings {
  hasApiKey: boolean;
  apiKeyHint: string | null;
  model: string;
  fallbackModel: string;
  ownNumber: string;
  dailyPerNumberCap: number;
  dailySmsPartCap: number;
  dailySpendCapUsd: number;
  subscriptionId: number;
  sendIntervalMs: number;
}

export type SettingsUpdate = Partial<Omit<GatewaySettings, "hasApiKey" | "apiKeyHint">>;

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
  kind: "receive" | "send" | "answer" | "command" | "ignore" | "expire" | "limit" | "error" | "service";
  text: string;
}

export type AskResult =
  | {
      ok: true;
      /** The whole cleaned-up answer. */
      text: string;
      /** What the first SMS would say. */
      firstSms: string;
      hasMore: boolean;
      model: string;
      latencyMs: number;
      costUsd: number;
      error: null;
    }
  | { ok: false; error: string };

interface SmsGatewayNative {
  getStatus(): GatewayStatus;
  getSettings(): GatewaySettings;
  setSettings(update: SettingsUpdate): void;
  setApiKey(key: string): void;
  clearApiKey(): void;
  getPermissions(): GatewayPermissions;
  start(): void;
  stop(): void;
  setPaused(paused: boolean): void;
  listSims(): Sim[];
  recentEvents(limit: number): GatewayEvent[];
  askAI(question: string): Promise<AskResult>;
  requestIgnoreBatteryOptimizations(): void;
  openAppSettings(): void;
}

export const SmsGateway = requireNativeModule<SmsGatewayNative>("SmsGateway");
