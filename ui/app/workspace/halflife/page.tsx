import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { getErrorMessage, useGetHalflifeReportsQuery, useGetHalflifeUsageQuery } from "@/lib/store";
import { useState } from "react";
import { ReportSheet } from "./views/reportSheet";
import { VerdictBadge } from "./views/verdictBadge";

const POLL_MS = 5000;
const ALL = "all";
const VERDICTS = ["OUTDATED", "CONTRADICTED", "CONFLICTING", "UNVERIFIABLE", "CURRENT", "NOT_CHECKED"];

function Stat({ title, value, hint }: { title: string; value: string | number; hint?: string }) {
	return (
		<Card>
			<CardHeader className="pb-1">
				<CardTitle className="text-muted-foreground text-xs font-medium">{title}</CardTitle>
			</CardHeader>
			<CardContent>
				<div className="text-2xl font-semibold">{value}</div>
				{hint && <p className="text-muted-foreground text-xs">{hint}</p>}
			</CardContent>
		</Card>
	);
}

/** Halflife's audits: what was checked, what it found, what it cost and what it did to the cache. */
export default function HalflifePage() {
	const [verdict, setVerdict] = useState(ALL);
	const [selected, setSelected] = useState<string | null>(null);
	const { data: usage, error } = useGetHalflifeUsageQuery(undefined, { pollingInterval: POLL_MS });
	const { data: list } = useGetHalflifeReportsQuery({ limit: 100, ...(verdict === ALL ? {} : { verdict }) }, { pollingInterval: POLL_MS });

	if (error) {
		return (
			<div className="mx-auto w-full max-w-5xl p-4">
				<Alert variant="destructive">
					<AlertDescription>{getErrorMessage(error)}</AlertDescription>
				</Alert>
			</div>
		);
	}
	const tenants = usage?.tenants ?? [];
	const credits = tenants.reduce((sum, t) => sum + t.tavily_credits, 0);
	const usd = tenants.reduce((sum, t) => sum + t.tavily_usd + t.llm_usd, 0);
	const requests = usage?.requests;

	return (
		<div className="mx-auto w-full max-w-6xl space-y-6 p-4" data-testid="halflife-page">
			<div className="flex items-center justify-between">
				<div>
					<h1 className="text-lg font-semibold">Halflife</h1>
					<p className="text-muted-foreground text-sm">Stale-fact audits of LLM answers, checked against live sources.</p>
				</div>
				<div className="flex items-center gap-2">
					{usage && <Badge variant="outline">mode: {usage.mode}</Badge>}
					{Object.entries(usage?.dependencies ?? {}).map(([name, d]) => (
						<Badge key={name} variant={d.state === "closed" ? "success" : "warning"} title={d.reason}>
							{name}: {d.state === "closed" ? "ok" : d.state}
						</Badge>
					))}
				</div>
			</div>

			<div className="grid grid-cols-2 gap-3 md:grid-cols-4">
				<Stat title="Answers captured" value={requests?.Captured ?? 0} hint={`${requests?.CachedHits ?? 0} served from cache`} />
				<Stat title="Verification spend" value={`$${usd.toFixed(3)}`} hint={`${credits} Tavily credits`} />
				<Stat title="Evicted from cache" value={(requests?.Invalidated ?? 0) + (requests?.Expired ?? 0)} hint={`${requests?.Invalidated ?? 0} stale, ${requests?.Expired ?? 0} expired`} />
				<Stat title="Kept off the cache" value={requests?.TombstoneBypasses ?? 0} hint={`${requests?.TombstoneReuses ?? 0} answered without a new audit`} />
			</div>

			{tenants.length > 0 && (
				<Table data-testid="halflife-tenants">
					<TableHeader>
						<TableRow>
							<TableHead>Tenant</TableHead>
							<TableHead>Audits</TableHead>
							<TableHead>Claims checked</TableHead>
							<TableHead>Credits</TableHead>
							<TableHead>Spend</TableHead>
							<TableHead>Spend cap</TableHead>
						</TableRow>
					</TableHeader>
					<TableBody>
						{tenants.map((t) => {
							const cap = usage?.spend?.[t.tenant_virtual_key_id];
							return (
								<TableRow key={t.tenant_virtual_key_id}>
									<TableCell>{t.tenant_virtual_key_name ?? t.tenant_virtual_key_id}</TableCell>
									<TableCell>{t.audits}</TableCell>
									<TableCell>{t.claims_checked}</TableCell>
									<TableCell>{t.tavily_credits}</TableCell>
									<TableCell>${(t.tavily_usd + t.llm_usd).toFixed(3)}</TableCell>
									<TableCell>{cap ? `$${cap.spent_usd.toFixed(3)} of $${cap.cap_usd.toFixed(3)}` : "—"}</TableCell>
								</TableRow>
							);
						})}
					</TableBody>
				</Table>
			)}

			<div className="space-y-2">
				<div className="flex items-center justify-between">
					<h2 className="text-sm font-medium">Reports</h2>
					<Select value={verdict} onValueChange={setVerdict}>
						<SelectTrigger className="w-44" aria-label="Filter by verdict" data-testid="halflife-verdict-filter">
							<SelectValue />
						</SelectTrigger>
						<SelectContent>
							<SelectItem value={ALL}>All verdicts</SelectItem>
							{VERDICTS.map((v) => (
								<SelectItem key={v} value={v}>
									{v.replace("_", " ")}
								</SelectItem>
							))}
						</SelectContent>
					</Select>
				</div>
				<Table data-testid="halflife-reports">
					<TableHeader>
						<TableRow>
							<TableHead>Time</TableHead>
							<TableHead>Verdict</TableHead>
							<TableHead>Tenant</TableHead>
							<TableHead>Model</TableHead>
							<TableHead>Claims</TableHead>
							<TableHead>Credits</TableHead>
							<TableHead>Outcome</TableHead>
						</TableRow>
					</TableHeader>
					<TableBody>
						{(list?.reports ?? []).map((r) => (
							<TableRow key={r.id} className="cursor-pointer" onClick={() => setSelected(r.id)}>
								<TableCell>{new Date(r.created_at).toLocaleTimeString()}</TableCell>
								<TableCell>
									<VerdictBadge verdict={r.verdict} />
								</TableCell>
								<TableCell>{r.tenant_virtual_key_id}</TableCell>
								<TableCell>
									{r.provider}/{r.model}
								</TableCell>
								<TableCell>
									{r.claims_checked} of {r.claims}
								</TableCell>
								<TableCell>{r.credits}</TableCell>
								<TableCell className="text-muted-foreground">{r.outcome ?? r.status}</TableCell>
							</TableRow>
						))}
						{list?.reports.length === 0 && (
							<TableRow>
								<TableCell colSpan={7} className="text-muted-foreground text-center">
									No reports yet. Send a request through the gateway.
								</TableCell>
							</TableRow>
						)}
					</TableBody>
				</Table>
			</div>
			<ReportSheet id={selected} onClose={() => setSelected(null)} />
		</div>
	);
}
