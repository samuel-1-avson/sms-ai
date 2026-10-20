// Pairs the phone with the server: scan the QR code printed by
// `admin gateway-pair` (or paste its JSON), test the connection, start.
import { Pairing } from "@smsai/shared";
import { CameraView, useCameraPermissions } from "expo-camera";
import { useRef, useState } from "react";
import { ScrollView, Text, TextInput, View } from "react-native";
import { SmsGateway } from "../modules/sms-gateway";
import { Button, Card, Note, styles, usePalette } from "./ui";

export function PairScreen({ onPaired }: { onPaired: () => void }) {
  const c = usePalette();
  const [permission, requestPermission] = useCameraPermissions();
  const [scanning, setScanning] = useState(false);
  const [pasted, setPasted] = useState("");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<string | null>(null);
  const handled = useRef(false);

  async function pair(text: string) {
    let parsed: Pairing;
    try {
      parsed = Pairing.parse(JSON.parse(text));
    } catch {
      setMessage("That is not a pairing code. Run `admin gateway-pair` on the server and scan its QR code.");
      return;
    }
    setBusy(true);
    setMessage("Checking the connection…");
    try {
      const res = await SmsGateway.testConnection(parsed.url, parsed.secret);
      if (!res.ok) {
        setMessage(`Could not connect: ${res.error ?? "unknown error"}`);
        return;
      }
      SmsGateway.configure(parsed.url, parsed.secret);
      SmsGateway.start();
      setMessage(null);
      onPaired();
    } finally {
      setBusy(false);
      handled.current = false;
    }
  }

  async function startScan() {
    if (!permission?.granted) {
      const p = await requestPermission();
      if (!p.granted) {
        setMessage("Camera permission is needed to scan. You can paste the code instead.");
        return;
      }
    }
    handled.current = false;
    setScanning(true);
  }

  return (
    <ScrollView contentContainerStyle={{ padding: 16, gap: 14 }}>
      <Card title="Pair with your server">
        <Note>
          On the server, run <Text style={styles.mono}>admin gateway-pair</Text>. It prints a QR code holding the server
          address and this phone's secret. Scan it here.
        </Note>
        {scanning ? (
          <View style={{ height: 300, borderRadius: 12, overflow: "hidden" }}>
            <CameraView
              style={{ flex: 1 }}
              barcodeScannerSettings={{ barcodeTypes: ["qr"] }}
              onBarcodeScanned={({ data }) => {
                if (handled.current) return;
                handled.current = true;
                setScanning(false);
                void pair(data);
              }}
            />
          </View>
        ) : (
          <Button label="Scan QR code" kind="primary" onPress={startScan} disabled={busy} />
        )}
        {scanning ? <Button label="Cancel" onPress={() => setScanning(false)} /> : null}
      </Card>

      <Card title="Or paste the code">
        <TextInput
          value={pasted}
          onChangeText={setPasted}
          placeholder='{"v":1,"url":"https://…","secret":"…"}'
          placeholderTextColor={c.muted}
          autoCapitalize="none"
          autoCorrect={false}
          multiline
          style={{ minHeight: 80, borderWidth: 1, borderColor: c.line, borderRadius: 10, padding: 10, color: c.ink, ...styles.mono }}
        />
        <Button label="Pair" onPress={() => pair(pasted.trim())} disabled={busy || !pasted.trim()} />
      </Card>

      {message ? <Text style={{ color: c.warn, fontSize: 14 }}>{message}</Text> : null}
    </ScrollView>
  );
}
