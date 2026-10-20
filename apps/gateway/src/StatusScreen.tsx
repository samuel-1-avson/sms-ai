// What the SMS AI is doing right now: service state, today's totals against
// the limits, recent activity, and a box to try a question without SMS.
import { useState } from "react";
import { ScrollView, Switch, Text, TextInput, View } from "react-native";
import { SmsGateway, type AskResult, type GatewayEvent } from "../modules/sms-gateway";
import { TestResult } from "./SettingsScreen";
import { Button, Card, Note, Row, usePalette, usePoll } from "./ui";

function ago(ms: number): string {
  if (!ms) return "never";
  const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}

export function StatusScreen() {
  const c = usePalette();
  const [status, refresh] = usePoll(() => SmsGateway.getStatus(), 2000);
  const [events] = usePoll(() => SmsGateway.recentEvents(25), 3000);
  const settings = SmsGateway.getSettings();

  const stateText = !status.running ? "Stopped" : status.paused ? "Paused" : status.lastError ? "Problem" : "Answering";
  const good = status.running && !status.paused && !status.lastError;

  return (
    <ScrollView contentContainerStyle={{ padding: 16, gap: 14 }} keyboardShouldPersistTaps="handled">
      <Card title="SMS AI" right={<Text style={{ color: good ? c.ok : c.warn, fontWeight: "700" }}>{stateText}</Text>}>
        {status.lastError ? <Text style={{ color: c.warn }}>{status.lastError}</Text> : null}
        {status.aiBreaker !== "closed" ? <Note>AI calls are paused for a few minutes after errors ({status.aiBreaker.replace(/^open: /, "")}).</Note> : null}
        <Row label="Last AI answer" value={ago(status.lastAnswerAt)} />
        <Row label="Texts waiting for an answer" value={String(status.waiting)} tone={status.waiting > 0 ? "warn" : undefined} />
        <Row label="Replies waiting to send" value={String(status.outboxPending + status.sending)} />
        <View style={{ flexDirection: "row", justifyContent: "space-between", alignItems: "center" }}>
          <Text style={{ color: c.ink, fontSize: 15 }}>Answer texts</Text>
          <Switch
            value={status.enabled}
            onValueChange={(v) => {
              if (v) SmsGateway.start();
              else SmsGateway.stop();
              refresh();
            }}
          />
        </View>
        <View style={{ flexDirection: "row", justifyContent: "space-between", alignItems: "center" }}>
          <Text style={{ color: c.ink, fontSize: 15 }}>Pause replies</Text>
          <Switch
            value={status.paused}
            onValueChange={(v) => {
              SmsGateway.setPaused(v);
              refresh();
            }}
          />
        </View>
        {status.paused ? <Note>Texts are kept and answered when you resume.</Note> : null}
      </Card>

      <Card title="Today">
        <Row label="Texts received" value={String(status.receivedToday)} />
        <Row label="Questions answered" value={String(status.answeredToday)} />
        <Row label="Replies sent" value={String(status.sentToday)} />
        {status.failedToday > 0 ? <Row label="Replies that failed to send" value={String(status.failedToday)} tone="warn" /> : null}
        <Row
          label="SMS parts used"
          value={`${status.smsPartsToday} / ${settings.dailySmsPartCap}`}
          tone={status.smsPartsToday >= settings.dailySmsPartCap ? "warn" : undefined}
        />
        <Row
          label="AI spend"
          value={`US$${status.aiCostToday.toFixed(4)} / ${settings.dailySpendCapUsd.toFixed(2)}`}
          tone={status.aiCostToday >= settings.dailySpendCapUsd ? "warn" : undefined}
        />
        {status.aiFailuresToday > 0 ? <Row label="AI errors" value={`${status.aiFailuresToday} of ${status.aiCallsToday} calls`} /> : null}
      </Card>

      <TryIt />

      <Card title="Recent activity">
        {events.length === 0 ? <Note>Nothing yet.</Note> : null}
        {events.map((e: GatewayEvent, i: number) => (
          <View key={`${e.at}-${i}`} style={{ flexDirection: "row", gap: 10 }}>
            <Text style={{ color: c.muted, fontSize: 12, width: 64 }}>{new Date(e.at).toLocaleTimeString()}</Text>
            <Text style={{ color: e.kind === "error" || e.kind === "limit" ? c.warn : c.ink, fontSize: 13, flex: 1 }}>{e.text}</Text>
          </View>
        ))}
      </Card>
    </ScrollView>
  );
}

/** Asks the AI exactly as a texter would, without sending an SMS. */
function TryIt() {
  const c = usePalette();
  const [q, setQ] = useState("");
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<AskResult | null>(null);
  return (
    <Card title="Try a question">
      <TextInput
        value={q}
        onChangeText={setQ}
        placeholder="e.g. How do I check my MoMo balance?"
        placeholderTextColor={c.muted}
        multiline
        style={{ minHeight: 60, borderWidth: 1, borderColor: c.line, borderRadius: 10, padding: 10, color: c.ink, fontSize: 15 }}
      />
      <Button
        label={busy ? "Asking…" : "Ask"}
        kind="primary"
        disabled={busy || q.trim() === ""}
        onPress={async () => {
          setBusy(true);
          try {
            setResult(await SmsGateway.askAI(q));
          } finally {
            setBusy(false);
          }
        }}
      />
      {result ? <TestResult result={result} /> : null}
      <Note>Uses your DeepInfra credit; no SMS is sent.</Note>
    </Card>
  );
}
