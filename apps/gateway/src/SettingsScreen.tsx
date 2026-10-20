// The DeepInfra API key, the AI models and the daily limits. Saving a key
// asks the AI a test question, so a working setup is proven before any texts
// are answered.
import { useState } from "react";
import { Alert, ScrollView, Text, TextInput, View } from "react-native";
import { SmsGateway, type AskResult, type GatewaySettings } from "../modules/sms-gateway";
import { Button, Card, Note, Row, styles, usePalette } from "./ui";

const TEST_QUESTION = "What is the capital of Ghana?";

export function SettingsScreen({ onReady }: { onReady?: () => void }) {
  const c = usePalette();
  const [settings, setSettings] = useState<GatewaySettings>(() => SmsGateway.getSettings());
  const [key, setKey] = useState("");
  const [busy, setBusy] = useState(false);
  const [test, setTest] = useState<AskResult | null>(null);
  const [message, setMessage] = useState<string | null>(null);
  const reload = () => setSettings(SmsGateway.getSettings());

  async function saveKey() {
    setMessage(null);
    setTest(null);
    try {
      SmsGateway.setApiKey(key);
    } catch (e) {
      setMessage((e as Error).message);
      return;
    }
    setKey("");
    reload();
    await runTest(true);
  }

  async function runTest(startAfter = false) {
    setBusy(true);
    try {
      const res = await SmsGateway.askAI(TEST_QUESTION);
      setTest(res);
      if (res.ok && startAfter) {
        SmsGateway.start();
        onReady?.();
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <ScrollView contentContainerStyle={{ padding: 16, gap: 14 }} keyboardShouldPersistTaps="handled">
      <Card title="DeepInfra API key">
        <Row
          label="Saved key"
          value={settings.hasApiKey ? (settings.apiKeyHint ?? "saved") : "none"}
          tone={settings.hasApiKey ? "ok" : "warn"}
        />
        <Note>
          The phone calls the AI with this key over the internet. Create one at deepinfra.com → Dashboard → API keys.
          It is stored encrypted on this phone only.
        </Note>
        <TextInput
          value={key}
          onChangeText={setKey}
          placeholder={settings.hasApiKey ? "Paste a new key to replace it" : "Paste your API key"}
          placeholderTextColor={c.muted}
          autoCapitalize="none"
          autoCorrect={false}
          secureTextEntry
          style={[inputStyle(c), styles.mono]}
        />
        <Button label={busy ? "Testing…" : "Save and test"} kind="primary" onPress={saveKey} disabled={busy || key.trim() === ""} />
        {settings.hasApiKey ? <Button label="Test again" onPress={() => runTest(false)} disabled={busy} /> : null}
        {test ? <TestResult result={test} /> : null}
        {message ? <Text style={{ color: c.warn }}>{message}</Text> : null}
      </Card>

      <Card title="Daily limits">
        <Note>These protect your airtime and your DeepInfra credit. When a limit is reached, texts wait until tomorrow.</Note>
        <NumberField
          label="Questions per person"
          value={settings.dailyPerNumberCap}
          onSave={(v) => SmsGateway.setSettings({ dailyPerNumberCap: Math.round(v) })}
          after={reload}
        />
        <NumberField
          label="SMS parts sent (all people)"
          value={settings.dailySmsPartCap}
          onSave={(v) => SmsGateway.setSettings({ dailySmsPartCap: Math.round(v) })}
          after={reload}
        />
        <NumberField
          label="AI spend (US$)"
          value={settings.dailySpendCapUsd}
          decimals
          onSave={(v) => SmsGateway.setSettings({ dailySpendCapUsd: v })}
          after={reload}
        />
      </Card>

      <Card title="This phone's number">
        <Note>Optional. Texts from this number are never answered, so the phone cannot end up replying to itself.</Note>
        <TextField
          value={settings.ownNumber}
          placeholder="e.g. 0244123456"
          keyboard="phone-pad"
          onSave={(v) => SmsGateway.setSettings({ ownNumber: v })}
          after={reload}
        />
      </Card>

      <Card title="AI models">
        <Row label="Main" value={settings.model} />
        <TextField value={settings.model} onSave={(v) => SmsGateway.setSettings({ model: v })} after={reload} />
        <Row label="Fallback" value={settings.fallbackModel || "none"} />
        <TextField
          value={settings.fallbackModel}
          placeholder="Leave empty for no fallback"
          onSave={(v) => SmsGateway.setSettings({ fallbackModel: v })}
          after={reload}
        />
        <Note>The fallback is asked once when the main model fails or returns nothing. Change these only if you know the DeepInfra model names.</Note>
      </Card>

      {settings.hasApiKey ? (
        <Button
          label="Remove API key"
          kind="danger"
          onPress={() =>
            Alert.alert("Remove the key?", "Texts will wait unanswered until a new key is saved.", [
              { text: "Cancel" },
              {
                text: "Remove",
                style: "destructive",
                onPress: () => {
                  SmsGateway.clearApiKey();
                  reload();
                },
              },
            ])
          }
        />
      ) : null}
    </ScrollView>
  );
}

export function TestResult({ result }: { result: AskResult }) {
  const c = usePalette();
  if (!result.ok) return <Text style={{ color: c.warn }}>Did not work: {result.error}</Text>;
  return (
    <View style={{ gap: 6 }}>
      <Text style={{ color: c.ok, fontWeight: "600" }}>
        Working · {(result.latencyMs / 1000).toFixed(1)} s · {result.model.split("/").pop()}
      </Text>
      <Text style={{ color: c.ink, fontSize: 15 }}>{result.firstSms}</Text>
      {result.hasMore ? <Note>(The rest is sent when the person texts MORE.)</Note> : null}
    </View>
  );
}

function inputStyle(c: ReturnType<typeof usePalette>) {
  return { borderWidth: 1, borderColor: c.line, borderRadius: 10, padding: 10, color: c.ink, fontSize: 15 };
}

function TextField({
  value,
  placeholder,
  keyboard,
  onSave,
  after,
}: {
  value: string;
  placeholder?: string;
  keyboard?: "default" | "phone-pad";
  onSave: (v: string) => void;
  after: () => void;
}) {
  const c = usePalette();
  const [draft, setDraft] = useState(value);
  const changed = draft.trim() !== value;
  return (
    <View style={{ flexDirection: "row", gap: 8, alignItems: "center" }}>
      <TextInput
        value={draft}
        onChangeText={setDraft}
        placeholder={placeholder}
        placeholderTextColor={c.muted}
        autoCapitalize="none"
        autoCorrect={false}
        keyboardType={keyboard ?? "default"}
        style={[inputStyle(c), { flex: 1 }]}
      />
      <Button
        label="Save"
        disabled={!changed}
        onPress={() => {
          onSave(draft.trim());
          after();
        }}
      />
    </View>
  );
}

function NumberField({
  label,
  value,
  decimals,
  onSave,
  after,
}: {
  label: string;
  value: number;
  decimals?: boolean;
  onSave: (v: number) => void;
  after: () => void;
}) {
  const c = usePalette();
  const shown = decimals ? value.toFixed(2) : String(value);
  const [draft, setDraft] = useState(shown);
  const parsed = Number(draft.replace(",", "."));
  const valid = draft.trim() !== "" && Number.isFinite(parsed) && parsed > 0;
  return (
    <View style={{ gap: 6 }}>
      <Text style={{ color: c.muted, fontSize: 14 }}>{label}</Text>
      <View style={{ flexDirection: "row", gap: 8, alignItems: "center" }}>
        <TextInput
          value={draft}
          onChangeText={setDraft}
          keyboardType={decimals ? "decimal-pad" : "number-pad"}
          style={[inputStyle(c), { flex: 1 }]}
        />
        <Button
          label="Save"
          disabled={!valid || draft === shown}
          onPress={() => {
            onSave(parsed);
            after();
          }}
        />
      </View>
    </View>
  );
}
