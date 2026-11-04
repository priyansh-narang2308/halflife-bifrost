import { Badge } from "@/components/ui/badge";

const VARIANTS: Record<string, "success" | "warning" | "destructive" | "secondary"> = {
	CURRENT: "success",
	OUTDATED: "destructive",
	CONTRADICTED: "destructive",
	CONFLICTING: "warning",
	UNVERIFIABLE: "secondary",
	NOT_CHECKED: "secondary",
	not_judged: "secondary",
};

export function VerdictBadge({ verdict }: { verdict?: string }) {
	if (!verdict) {
		return <span className="text-muted-foreground">—</span>;
	}
	return <Badge variant={VARIANTS[verdict] ?? "secondary"}>{verdict.replace("_", " ")}</Badge>;
}
