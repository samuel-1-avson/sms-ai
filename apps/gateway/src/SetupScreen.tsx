// The checklist that keeps the SMS AI answering: permissions, battery
// optimisation, the SIM that answers, and the Android SMS rate limit.
import { useState } from "react";
import { Alert, Linking, PermissionsAndroid, Platform, ScrollView, Text, View, type Permission } from "react-native";
import { SmsGateway } from "../modules/sms-gateway";
import { Button, Card, Note, Row, styles, usePalette, usePoll } from "./ui";

const PERMISSIONS = [
  PermissionsAndroid.PERMISSIONS.RECEIVE_SMS,
  PermissionsAndroid.PERMISSIONS.SEND_SMS,
  PermissionsAndroid.PERMISSIONS.READ_PHONE_STATE,
  ...(Platform.OS === "android" && Number(Platform.Version) >= 33 ? [PermissionsAndroid.PERMISSIONS.POST_NOTIFICATIONS] : []),
] as Permission[];

export async function requestGatewayPermissions(): Promise<boolean> {
  const result = await PermissionsAndroid.requestMultiple(PERMISSIONS);
  return Object.values(result).every((r) => r === PermissionsAndroid.RESULTS.GRANTED);
}

export function SetupScreen() {
  const c = usePalette();
  const [perms, refresh] = usePoll(() => SmsGateway.getPermissions(), 2000);
  const [sims, setSims] = useState(() => SmsGateway.listSims());
  const settings = SmsGateway.getSettings();
  const [sim, setSim] = useState(settings.subscriptionId);
  const [interval, setIntervalMs] = useState(settings.sendIntervalMs);

  const yes = (v: boolean) => (v ? "Granted" : "Missing");
  const tone = (v: boolean): "ok" | "warn" => (v ? "ok" : "warn");

  return (
    <ScrollView contentContainerStyle={{ padding: 16, gap: 14 }}>
      <Card title="Permissions">
        <Row label="Receive SMS" value={yes(perms.receiveSms)} tone={tone(perms.receiveSms)} />
        <Row label="Send SMS" value={yes(perms.sendSms)} tone={tone(perms.sendSms)} />
        <Row label="Phone state (SIM list)" value={yes(perms.readPhoneState)} tone={tone(perms.readPhoneState)} />
        <Row label="Notifications" value={yes(perms.notifications)} tone={tone(perms.notifications)} />
        <Button
          label="Grant permissions"
          kind="primary"
          onPress={async () => {
            const ok = await requestGatewayPermissions();
            refresh();
            setSims(SmsGateway.listSims());
            if (!ok) {
              Alert.alert("Some permissions are missing", "Open the app settings and allow SMS and phone permissions.", [
                { text: "Cancel" },
                { text: "Open settings", onPress: () => SmsGateway.openAppSettings() },
              ]);
            }
          }}
        />
      </Card>

      <Card title="Stay awake">
        <Row
          label="Battery optimisation"
          value={perms.ignoringBatteryOptimizations ? "Off (good)" : "On: answers may stop"}
          tone={tone(perms.ignoringBatteryOptimizations)}
        />
        <Button label="Turn off battery optimisation" onPress={() => SmsGateway.requestIgnoreBatteryOptimizations()} />
        <Note>
          Tecno, Infinix, itel, Xiaomi and Samsung phones also have their own "auto-start" or "sleeping apps" lists. Allow
          this app to auto-start and never put it to sleep. Keep the phone charging and on Wi-Fi or mobile data.
        </Note>
        <Button label="Open app settings" onPress={() => SmsGateway.openAppSettings()} />
      </Card>

      <Card title="SIM that answers">
        {sims.length === 0 ? <Note>Grant the phone-state permission to choose a SIM. The default SMS SIM is used.</Note> : null}
        {[{ subscriptionId: -1, slot: -1, label: "Default SMS SIM", carrier: "" }, ...sims].map((s) => (
          <Button
            key={s.subscriptionId}
            label={`${sim === s.subscriptionId ? "✓ " : ""}${s.label}${s.carrier ? ` · ${s.carrier}` : ""}${s.slot >= 0 ? ` (slot ${s.slot + 1})` : ""}`}
            onPress={() => {
              SmsGateway.setSettings({ subscriptionId: s.subscriptionId });
              setSim(s.subscriptionId);
            }}
          />
        ))}
        <Note>Texts that arrive on another SIM are ignored.</Note>
      </Card>

      <Card title="Sending pace">
        <Row label="Gap between replies" value={`${(interval / 1000).toFixed(1)} s`} />
        <View style={{ flexDirection: "row", gap: 8 }}>
          {[1000, 2000, 5000].map((ms) => (
            <Button
              key={ms}
              label={`${ms / 1000} s`}
              kind={interval === ms ? "primary" : "plain"}
              style={{ flex: 1 }}
              onPress={() => {
                SmsGateway.setSettings({ sendIntervalMs: ms });
                setIntervalMs(ms);
              }}
            />
          ))}
        </View>
        <Note>
          Android asks for confirmation after about 30 texts in 30 minutes. Lift that limit once from a computer with USB
          debugging on:
        </Note>
        <Text selectable style={[styles.mono, { color: c.ink }]}>
          adb shell settings put global sms_outgoing_check_max_count 100000
        </Text>
        <Button label="How to enable USB debugging" onPress={() => Linking.openURL("https://developer.android.com/studio/debug/dev-options")} />
      </Card>

    </ScrollView>
  );
}
