// SMS AI gateway: turns this phone's SIM into the SMS front door for the
// AI. The work happens in a native foreground service; these screens pair
// the phone, show what it is doing and walk through the setup.
import { StatusBar } from "expo-status-bar";
import { useEffect, useState } from "react";
import { Pressable, SafeAreaView, StyleSheet, Text, View } from "react-native";
import { SmsGateway } from "./modules/sms-gateway";
import { PairScreen } from "./src/PairScreen";
import { SetupScreen, requestGatewayPermissions } from "./src/SetupScreen";
import { StatusScreen } from "./src/StatusScreen";
import { usePalette } from "./src/ui";

type Tab = "status" | "setup";

export default function App() {
  const c = usePalette();
  const [paired, setPaired] = useState(() => SmsGateway.getStatus().configured);
  const [tab, setTab] = useState<Tab>("status");

  useEffect(() => {
    // Ask once on first launch; the Setup tab shows anything still missing.
    void requestGatewayPermissions();
  }, []);

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: c.bg }}>
      <StatusBar style="auto" />
      <View style={[styles.header, { borderColor: c.line }]}>
        <Text style={[styles.title, { color: c.ink }]}>SMS AI Gateway</Text>
        {paired ? (
          <View style={styles.tabs}>
            {(["status", "setup"] as const).map((t) => (
              <Pressable
                key={t}
                onPress={() => setTab(t)}
                accessibilityRole="tab"
                accessibilityState={{ selected: tab === t }}
                style={[styles.tab, { backgroundColor: tab === t ? c.accent : "transparent", borderColor: c.line }]}
              >
                <Text style={{ color: tab === t ? c.accentInk : c.ink, fontWeight: "600" }}>{t === "status" ? "Status" : "Setup"}</Text>
              </Pressable>
            ))}
          </View>
        ) : null}
      </View>
      {!paired ? (
        <PairScreen onPaired={() => setPaired(true)} />
      ) : tab === "status" ? (
        <StatusScreen />
      ) : (
        <SetupScreen
          onUnpaired={() => {
            setPaired(false);
            setTab("status");
          }}
        />
      )}
    </SafeAreaView>
  );
}

const styles = StyleSheet.create({
  header: { paddingHorizontal: 16, paddingTop: 36, paddingBottom: 12, gap: 10, borderBottomWidth: StyleSheet.hairlineWidth },
  title: { fontSize: 20, fontWeight: "700" },
  tabs: { flexDirection: "row", gap: 8 },
  tab: { paddingVertical: 7, paddingHorizontal: 16, borderRadius: 999, borderWidth: 1 },
});
