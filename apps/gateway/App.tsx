// SMS AI: this phone answers text messages with an AI. People text the
// phone's SIM (no data needed on their side); the phone asks the AI over the
// internet and texts the answer back. The work happens in a native foreground
// service; these screens set it up and show what it is doing.
import { StatusBar } from "expo-status-bar";
import { useEffect, useState } from "react";
import { Pressable, SafeAreaView, StyleSheet, Text, View } from "react-native";
import { SmsGateway } from "./modules/sms-gateway";
import { SettingsScreen } from "./src/SettingsScreen";
import { SetupScreen, requestGatewayPermissions } from "./src/SetupScreen";
import { StatusScreen } from "./src/StatusScreen";
import { usePalette } from "./src/ui";

const TABS = [
  ["status", "Status"],
  ["settings", "Settings"],
  ["setup", "Phone setup"],
] as const;
type Tab = (typeof TABS)[number][0];

export default function App() {
  const c = usePalette();
  // Without an API key there is nothing to run: start on Settings.
  const [tab, setTab] = useState<Tab>(() => (SmsGateway.getStatus().configured ? "status" : "settings"));

  useEffect(() => {
    // Ask once on first launch; the Phone setup tab shows anything still missing.
    void requestGatewayPermissions();
  }, []);

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: c.bg }}>
      <StatusBar style="auto" />
      <View style={[styles.header, { borderColor: c.line }]}>
        <Text style={[styles.title, { color: c.ink }]}>SMS AI</Text>
        <View style={styles.tabs}>
          {TABS.map(([t, label]) => (
            <Pressable
              key={t}
              onPress={() => setTab(t)}
              accessibilityRole="tab"
              accessibilityState={{ selected: tab === t }}
              style={[styles.tab, { backgroundColor: tab === t ? c.accent : "transparent", borderColor: c.line }]}
            >
              <Text style={{ color: tab === t ? c.accentInk : c.ink, fontWeight: "600" }}>{label}</Text>
            </Pressable>
          ))}
        </View>
      </View>
      {tab === "status" ? <StatusScreen /> : tab === "settings" ? <SettingsScreen onReady={() => setTab("status")} /> : <SetupScreen />}
    </SafeAreaView>
  );
}

const styles = StyleSheet.create({
  header: { paddingHorizontal: 16, paddingTop: 36, paddingBottom: 12, gap: 10, borderBottomWidth: StyleSheet.hairlineWidth },
  title: { fontSize: 20, fontWeight: "700" },
  tabs: { flexDirection: "row", gap: 8, flexWrap: "wrap" },
  tab: { paddingVertical: 7, paddingHorizontal: 14, borderRadius: 999, borderWidth: 1 },
});
