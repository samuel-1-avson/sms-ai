// Small shared UI pieces and the colour palette (light and dark).
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Pressable, StyleSheet, Text, View, useColorScheme, type ViewStyle } from "react-native";

const light = {
  bg: "#f4f1ea",
  card: "#ffffff",
  ink: "#1d1b16",
  muted: "#6b665c",
  line: "#e2ddd2",
  accent: "#0f6b4f",
  accentInk: "#ffffff",
  warn: "#a3470f",
  ok: "#1f7a4d",
};
const dark: typeof light = {
  bg: "#15140f",
  card: "#1f1d17",
  ink: "#ece8df",
  muted: "#a39e92",
  line: "#34312a",
  accent: "#3fae86",
  accentInk: "#0d1a14",
  warn: "#e08a52",
  ok: "#57c08d",
};
export type Palette = typeof light;

export function usePalette(): Palette {
  return useColorScheme() === "dark" ? dark : light;
}

/** Re-runs `read` every `ms` while mounted. */
export function usePoll<T>(read: () => T, ms: number): [T, () => void] {
  const [value, setValue] = useState(read);
  const readRef = useRef(read);
  readRef.current = read;
  useEffect(() => {
    const t = setInterval(() => setValue(readRef.current()), ms);
    return () => clearInterval(t);
  }, [ms]);
  return [value, () => setValue(readRef.current())];
}

export function Card({ title, children, right }: { title?: string; children: ReactNode; right?: ReactNode }) {
  const c = usePalette();
  return (
    <View style={[styles.card, { backgroundColor: c.card, borderColor: c.line }]}>
      {title ? (
        <View style={styles.cardHead}>
          <Text style={[styles.cardTitle, { color: c.ink }]}>{title}</Text>
          {right}
        </View>
      ) : null}
      {children}
    </View>
  );
}

export function Button({
  label,
  onPress,
  kind = "plain",
  disabled,
  style,
}: {
  label: string;
  onPress: () => void;
  kind?: "primary" | "plain" | "danger";
  disabled?: boolean;
  style?: ViewStyle;
}) {
  const c = usePalette();
  const bg = kind === "primary" ? c.accent : c.card;
  const fg = kind === "primary" ? c.accentInk : kind === "danger" ? c.warn : c.ink;
  return (
    <Pressable
      accessibilityRole="button"
      onPress={onPress}
      disabled={disabled}
      style={({ pressed }) => [
        styles.button,
        { backgroundColor: bg, borderColor: kind === "primary" ? c.accent : c.line, opacity: disabled ? 0.5 : pressed ? 0.8 : 1 },
        style,
      ]}
    >
      <Text style={[styles.buttonText, { color: fg }]}>{label}</Text>
    </Pressable>
  );
}

export function Row({ label, value, tone }: { label: string; value: string; tone?: "ok" | "warn" }) {
  const c = usePalette();
  return (
    <View style={styles.row}>
      <Text style={[styles.rowLabel, { color: c.muted }]}>{label}</Text>
      <Text style={[styles.rowValue, { color: tone === "ok" ? c.ok : tone === "warn" ? c.warn : c.ink }]} numberOfLines={2}>
        {value}
      </Text>
    </View>
  );
}

export function Note({ children }: { children: ReactNode }) {
  const c = usePalette();
  return <Text style={[styles.note, { color: c.muted }]}>{children}</Text>;
}

export const styles = StyleSheet.create({
  card: { borderWidth: 1, borderRadius: 14, padding: 14, gap: 10 },
  cardHead: { flexDirection: "row", justifyContent: "space-between", alignItems: "center" },
  cardTitle: { fontSize: 15, fontWeight: "600" },
  button: { borderWidth: 1, borderRadius: 10, paddingVertical: 11, paddingHorizontal: 14, alignItems: "center" },
  buttonText: { fontSize: 15, fontWeight: "600" },
  row: { flexDirection: "row", justifyContent: "space-between", gap: 12 },
  rowLabel: { fontSize: 14 },
  rowValue: { fontSize: 14, fontWeight: "500", flexShrink: 1, textAlign: "right" },
  note: { fontSize: 13, lineHeight: 18 },
  mono: { fontFamily: "monospace", fontSize: 12 },
});
