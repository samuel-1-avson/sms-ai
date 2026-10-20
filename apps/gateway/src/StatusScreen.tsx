// What the gateway is doing right now: service state, queues and recent activity.
import { ScrollView, Switch, Text, View } from "react-native";
import { SmsGateway, type GatewayEvent } from "../modules/sms-gateway";
import { Button, Card, Note, Row, usePalette, usePoll } from "./ui";

function ago(ms: number): string {
  if (!ms) return "never";
  const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  return `${Math.round(s / 3600)} h ago`;
}

export function StatusScreen() {
  const c = usePalette();
  const [status, refresh] = usePoll(() => SmsGateway.getStatus(), 2000);
  const [events] = usePoll(() => SmsGateway.recentEvents(25), 3000);

  const online = status.running && !status.lastError && Date.now() - status.lastContactAt < 120_000;
  const stateText = !status.running ? "Stopped" : status.lastError ? "Problem" : online ? "Online" : "Connecting…";

  return (
    <ScrollView contentContainerStyle={{ padding: 16, gap: 14 }}>
      <Card
        title="Gateway"
        right={<Text style={{ color: online ? c.ok : c.warn, fontWeight: "700" }}>{stateText}</Text>}
      >
        {status.lastError ? <Text style={{ color: c.warn }}>{status.lastError}</Text> : null}
        <Row label="Server" value={status.serverUrl ?? "-"} />
        <Row label="Last contact" value={ago(status.lastContactAt)} />
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
        {status.paused ? <Note>Texts are still forwarded; replies wait on the server until you resume.</Note> : null}
      </Card>

      <Card title="Today">
        <Row label="Texts received" value={String(status.receivedToday)} />
        <Row label="Replies sent" value={String(status.sentToday)} />
        <Row label="Waiting to forward" value={String(status.inboxPending)} tone={status.inboxPending > 0 ? "warn" : undefined} />
        <Row label="Replies waiting to send" value={String(status.outboxPending + status.sending)} />
        <Row label="Results waiting to report" value={String(status.statusPending)} />
      </Card>

      <Card title="Recent activity">
        {events.length === 0 ? <Note>Nothing yet.</Note> : null}
        {events.map((e: GatewayEvent, i: number) => (
          <View key={`${e.at}-${i}`} style={{ flexDirection: "row", gap: 10 }}>
            <Text style={{ color: c.muted, fontSize: 12, width: 64 }}>{new Date(e.at).toLocaleTimeString()}</Text>
            <Text style={{ color: e.kind === "error" ? c.warn : c.ink, fontSize: 13, flex: 1 }}>{e.text}</Text>
          </View>
        ))}
      </Card>

      <Button label="Refresh" onPress={refresh} />
    </ScrollView>
  );
}
