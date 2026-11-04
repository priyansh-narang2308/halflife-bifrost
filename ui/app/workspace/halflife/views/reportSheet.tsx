import { Sheet, SheetContent, SheetTitle } from "@/components/ui/sheet";
import { useGetHalflifeReportQuery } from "@/lib/store";
import type { HalflifeClaim, HalflifeQuote } from "@/lib/types/halflife";
import { VerdictBadge } from "./verdictBadge";

function Quote({ label, quote }: { label: string; quote?: HalflifeQuote }) {
	if (!quote?.text) {
		return null;
	}
	return (
		<div className="text-sm">
			<span className="text-muted-foreground">{label}: </span>
			<q>{quote.text}</q>
			{quote.url && (
				<a className="ml-2 underline" href={quote.url} target="_blank" rel="noreferrer noopener">
					source
				</a>
			)}
			{!quote.verified && <span className="text-muted-foreground ml-2">(not found verbatim)</span>}
			{quote.flagged && <span className="text-destructive ml-2">(flagged source)</span>}
		</div>
	);
}

function Claim({ claim }: { claim: HalflifeClaim }) {
	return (
		<div className="space-y-2 rounded-sm border p-3" data-testid="halflife-claim">
			<div className="flex items-start justify-between gap-3">
				<p className="text-sm font-medium">{claim.text}</p>
				<VerdictBadge verdict={claim.verdict ?? (claim.status === "not_checked" ? "NOT_CHECKED" : undefined)} />
			</div>
			<p className="text-muted-foreground text-xs">
				{[claim.category, claim.temporal_scope, claim.volatility_band, claim.priority].filter(Boolean).join(" · ")}
				{claim.evidence ? ` · ${claim.evidence.credits} credit${claim.evidence.credits === 1 ? "" : "s"}${claim.evidence.reused ? " (reused search)" : ""}` : ""}
			</p>
			{claim.observed_value && <p className="text-sm">Current value: {claim.observed_value}</p>}
			{(claim.verdict_reason || claim.status_reason) && <p className="text-muted-foreground text-sm">{claim.verdict_reason || claim.status_reason}</p>}
			<Quote label="Evidence" quote={claim.quote} />
			<Quote label="It used to hold" quote={claim.history_quote} />
			<Quote label="Other side" quote={claim.other_quote} />
			{claim.evidence && claim.evidence.sources.length > 0 && (
				<ul className="text-xs">
					{claim.evidence.sources.map((s) => (
						<li key={s.url} className="truncate">
							<a className="underline" href={s.url} target="_blank" rel="noreferrer noopener">
								{s.title || s.url}
							</a>
							{s.published_date && <span className="text-muted-foreground"> · {s.published_date}</span>}
							{s.flagged && <span className="text-destructive"> · flagged: {s.flag_reason}</span>}
						</li>
					))}
				</ul>
			)}
		</div>
	);
}

/** One report: the answer, and each claim with its verdict, evidence and cost. */
export function ReportSheet({ id, onClose }: { id: string | null; onClose: () => void }) {
	const { data: report, isFetching } = useGetHalflifeReportQuery(id ?? "", { skip: !id });
	return (
		<Sheet open={!!id} onOpenChange={(open) => !open && onClose()}>
			<SheetContent className="w-full overflow-y-auto p-4 sm:max-w-2xl">
				<SheetTitle>{isFetching && !report ? "Loading report" : "Report"}</SheetTitle>
				{report && (
					<div className="mt-4 space-y-4">
						<div className="flex items-center gap-3">
							<VerdictBadge verdict={report.verdict} />
							<span className="text-muted-foreground text-sm">
								{report.outcome} · {report.answer.provider}/{report.answer.model} · {report.tenant.virtual_key_name ?? report.tenant.virtual_key_id}
							</span>
						</div>
						{report.outcome_reason && <p className="text-sm">{report.outcome_reason}</p>}
						{report.answer.question && <p className="text-sm font-medium">{report.answer.question}</p>}
						{report.answer.text_redacted ? (
							<p className="text-muted-foreground text-sm">The answer text is not persisted.</p>
						) : (
							<p className="bg-muted rounded-sm p-3 text-sm whitespace-pre-wrap">{report.answer.text}</p>
						)}
						{report.claims.map((claim, i) => (
							<Claim key={`${claim.span}-${i}`} claim={claim} />
						))}
					</div>
				)}
			</SheetContent>
		</Sheet>
	);
}
