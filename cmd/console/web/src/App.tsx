import { useCallback, useEffect, useState } from "react";
import { AnswerPanel } from "./AnswerPanel";

type Sources = { state_table: string; tracker: string; workflow: string };

type Ticket = {
  issue_key: string;
  state: string;
  terminal_code?: string;
  attempt?: number;
  workflow_run_id?: string;
  queued_at?: string;
  claimed_at?: string;
  completed_at?: string;
  open_question: boolean;
  next_actor: string;
  ticket_url: string;
  workflow_run_url?: string;
  clarifications?: number;
};

type Overview = {
  generated_at: string;
  sources: Sources;
  tickets: Ticket[] | null;
  ingest_cursor?: string;
  pending_slot: boolean;
};

type TimelineEvent = {
  kind: string;
  at?: string;
  comment_id?: string;
  body: string;
  url?: string;
  run_id?: string;
  code?: string;
};

type JobNode = {
  name: string;
  status: string;
  conclusion?: string;
  started_at?: string;
  url?: string;
};

type TicketDetail = {
  issue_key: string;
  generated_at: string;
  sources: Sources;
  current?: Ticket;
  timeline: TimelineEvent[] | null;
  current_jobs?: JobNode[];
  can_answer?: boolean;
  answering_reason?: string;
  question_comment_id?: string;
};

export function App() {
  const [overview, setOverview] = useState<Overview | null>(null);
  const [overviewError, setOverviewError] = useState<string>("");
  const [selected, setSelected] = useState<string>("");
  const [detail, setDetail] = useState<TicketDetail | null>(null);
  const [detailError, setDetailError] = useState<string>("");

  const loadOverview = useCallback(async () => {
    try {
      const response = await fetch("/api/overview");
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      setOverview(await response.json());
      setOverviewError("");
    } catch (cause) {
      setOverviewError(String(cause));
    }
  }, []);

  const loadDetail = useCallback(async (key: string) => {
    setDetail(null);
    setDetailError("");
    try {
      const response = await fetch(`/api/tickets/${encodeURIComponent(key)}`);
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      setDetail(await response.json());
    } catch (cause) {
      setDetailError(String(cause));
    }
  }, []);

  useEffect(() => {
    loadOverview();
    const timer = setInterval(loadOverview, 30_000);
    return () => clearInterval(timer);
  }, [loadOverview]);

  useEffect(() => {
    if (!selected) return;
    loadDetail(selected);
    const timer = setInterval(() => loadDetail(selected), 30_000);
    return () => clearInterval(timer);
  }, [selected, loadDetail]);

  return (
    <div className="min-h-screen bg-solid-gray-50">
      <header className="bg-white border-b-4 border-blue-900">
        <div className="mx-auto max-w-screen-xl px-6 py-4 flex items-baseline gap-4">
          <h1 className="text-std-24B-150 text-solid-gray-900">
            チケット自動処理 運用コンソール
          </h1>
          <span className="text-std-14N-170 text-solid-gray-536">
            進行の閲覧と、待機中の質問への回答
          </span>
        </div>
      </header>

      <main className="mx-auto max-w-screen-xl px-6 py-6 flex flex-col gap-6">
        {overviewError && (
          <Banner tone="error" title="一覧を取得できません" body={overviewError} />
        )}
        {overview && <SourceBanners sources={overview.sources} />}

        <section aria-label="チケット一覧" className="bg-white border border-solid-gray-300 rounded-8">
          <div className="px-5 py-3 border-b border-solid-gray-300 flex items-baseline justify-between">
            <h2 className="text-std-18B-160 text-solid-gray-900">チケット一覧</h2>
            {overview && (
              <span className="text-std-14N-170 text-solid-gray-536">
                取得時刻 {formatTime(overview.generated_at)}
              </span>
            )}
          </div>
          <TicketTable
            tickets={overview?.tickets ?? []}
            stateTable={overview ? overview.sources.state_table : "loading"}
            selected={selected}
            onSelect={(key) => setSelected(key)}
          />
        </section>

        {selected && (
          <section aria-label="チケット詳細" className="bg-white border border-solid-gray-300 rounded-8">
            <div className="px-5 py-3 border-b border-solid-gray-300 flex items-baseline gap-3">
              <h2 className="text-std-18B-160 text-solid-gray-900">{selected} の進行</h2>
              {detail?.current && <StateChip ticket={detail.current} />}
            </div>
            {detailError && (
              <div className="p-5">
                <Banner tone="error" title="詳細を取得できません" body={detailError} />
              </div>
            )}
            {!detail && !detailError && (
              <p className="p-5 text-std-16N-170 text-solid-gray-536">読み込み中…</p>
            )}
            {detail && <TicketDetailView detail={detail} onAnswered={() => loadDetail(selected)} />}
          </section>
        )}
      </main>
    </div>
  );
}

function SourceBanners({ sources }: { sources: Sources }) {
  const rows: Array<[string, string]> = [
    ["状態の台帳", sources.state_table],
    ["トラッカー", sources.tracker],
    ["実行基盤", sources.workflow],
  ];
  const broken = rows.filter(([, state]) => state.startsWith("unavailable"));
  if (broken.length === 0) return null;
  return (
    <div className="flex flex-col gap-2">
      {broken.map(([name, state]) => (
        <Banner
          key={name}
          tone="warning"
          title={`${name} が読めません — この画面の該当部分は不明として表示しています`}
          body={state}
        />
      ))}
    </div>
  );
}

function Banner({ tone, title, body }: { tone: "error" | "warning"; title: string; body?: string }) {
  const palette =
    tone === "error"
      ? "border-error-1 bg-red-50 text-error-1"
      : "border-warning-yellow-2 bg-yellow-50 text-solid-gray-800";
  return (
    <div role="alert" className={`border-l-4 px-4 py-3 rounded-4 ${palette}`}>
      <p className="text-std-16B-170">{title}</p>
      {body && <p className="text-std-14N-170 text-solid-gray-700 break-all">{body}</p>}
    </div>
  );
}

function TicketTable({
  tickets,
  stateTable,
  selected,
  onSelect,
}: {
  tickets: Ticket[];
  stateTable: string;
  selected: string;
  onSelect: (key: string) => void;
}) {
  if (stateTable === "loading") {
    return <p className="p-5 text-std-16N-170 text-solid-gray-536">読み込み中…</p>;
  }
  if (stateTable.startsWith("unavailable")) {
    return (
      <p className="p-5 text-std-16N-170 text-solid-gray-700">
        状態の台帳が読めないため不明です (「ありません」ではありません)。
      </p>
    );
  }
  if (tickets.length === 0) {
    return <p className="p-5 text-std-16N-170 text-solid-gray-536">進行中の記録はありません。</p>;
  }
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left">
        <thead>
          <tr className="text-std-14B-170 text-solid-gray-700 border-b border-solid-gray-300">
            <th className="px-5 py-2">チケット</th>
            <th className="px-5 py-2">状態</th>
            <th className="px-5 py-2">次に行動する人</th>
            <th className="px-5 py-2">受付</th>
            <th className="px-5 py-2">質問</th>
            <th className="px-5 py-2">リンク</th>
          </tr>
        </thead>
        <tbody>
          {tickets.map((ticket) => (
            <tr
              key={ticket.issue_key}
              className={`border-b border-solid-gray-200 cursor-pointer hover:bg-blue-50 ${
                selected === ticket.issue_key ? "bg-blue-50" : ""
              }`}
              onClick={() => onSelect(ticket.issue_key)}
            >
              <td className="px-5 py-3 text-std-16B-170 text-blue-900">{ticket.issue_key}</td>
              <td className="px-5 py-3">
                <StateChip ticket={ticket} />
              </td>
              <td className="px-5 py-3 text-std-16N-170">{ticket.next_actor}</td>
              <td className="px-5 py-3 text-std-14N-170 text-solid-gray-700">
                {formatTime(ticket.queued_at)}
              </td>
              <td className="px-5 py-3 text-std-14N-170">
                {ticket.clarifications ? `${ticket.clarifications} 回` : "なし"}
              </td>
              <td className="px-5 py-3 text-std-14N-170">
                <a
                  className="text-blue-900 underline mr-3"
                  href={ticket.ticket_url}
                  target="_blank"
                  rel="noreferrer"
                  onClick={(event) => event.stopPropagation()}
                >
                  チケット
                </a>
                {ticket.workflow_run_url && (
                  <a
                    className="text-blue-900 underline"
                    href={ticket.workflow_run_url}
                    target="_blank"
                    rel="noreferrer"
                    onClick={(event) => event.stopPropagation()}
                  >
                    実行履歴
                  </a>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function StateChip({ ticket }: { ticket: Ticket }) {
  const [label, palette] = stateLabel(ticket);
  return (
    <span className={`inline-block px-3 py-1 rounded-full text-std-14B-170 ${palette}`}>
      {label}
    </span>
  );
}

function stateLabel(ticket: Ticket): [string, string] {
  if (ticket.state === "terminal") {
    if (ticket.terminal_code === "success") return ["完了", "bg-green-100 text-success-1"];
    return [`失敗 (${ticket.terminal_code ?? "不明"})`, "bg-red-100 text-error-1"];
  }
  if (ticket.open_question || ticket.state === "awaiting_answer")
    return ["回答待ち", "bg-yellow-100 text-solid-gray-800"];
  if (ticket.state === "claimed") return ["処理中", "bg-blue-100 text-blue-1000"];
  if (ticket.state === "queued") return ["取り込み待ち", "bg-solid-gray-100 text-solid-gray-700"];
  return [ticket.state || "不明", "bg-solid-gray-100 text-solid-gray-700"];
}

function TicketDetailView({ detail, onAnswered }: { detail: TicketDetail; onAnswered: () => void }) {
  // The ledger, not the comment stream, says which comment is the current
  // question: a question-shaped comment posted by anyone else never gets a
  // panel, because the server only names the comment the engine is waiting
  // on. The timeline supplies that comment's body for rendering.
  const question = detail.question_comment_id
    ? (detail.timeline ?? []).find(
        (event) => event.kind === "question" && event.comment_id === detail.question_comment_id,
      ) ?? null
    : null;
  return (
    <div className="p-5 flex flex-col gap-6">
      <SourceBanners sources={detail.sources} />
      {question && question.comment_id && detail.can_answer && (
        <AnswerPanel
          issueKey={detail.issue_key}
          commentID={question.comment_id}
          body={question.body}
          onAnswered={onAnswered}
        />
      )}
      {question && !detail.can_answer && (
        <Banner
          tone="warning"
          title={
            detail.answering_reason === "key_owner_unknown"
              ? "この画面からの回答は今は使えません"
              : "この画面の鍵では回答できません"
          }
          body={
            detail.answering_reason === "key_owner_unknown"
              ? "起動時にトラッカーの鍵の持ち主を確認できませんでした。鍵を確認してコンソールを再起動するか、Backlog のコメントで回答してください。"
              : "自動処理が回答を受け付けるのは、このチケットの許可回答者だけです。回答は Backlog のコメントとして、許可回答者本人が投稿してください。"
          }
        />
      )}

      {detail.current_jobs && detail.current_jobs.length > 0 && (
        <div>
          <h3 className="text-std-16B-170 text-solid-gray-900 mb-2">現在の実行の段階</h3>
          <ol className="flex flex-col gap-1">
            {detail.current_jobs.map((job) => (
              <li key={job.name} className="flex items-center gap-3">
                <JobMark job={job} />
                <a
                  className="text-std-16N-170 text-blue-900 underline"
                  href={job.url}
                  target="_blank"
                  rel="noreferrer"
                >
                  {jobLabel(job.name)}
                </a>
                <span className="text-std-14N-170 text-solid-gray-536">
                  {jobStateLabel(job)}
                </span>
              </li>
            ))}
          </ol>
        </div>
      )}

      <div>
        <h3 className="text-std-16B-170 text-solid-gray-900 mb-2">これまでの出来事</h3>
        {!detail.timeline || detail.timeline.length === 0 ? (
          <p className="text-std-16N-170 text-solid-gray-536">
            {detail.sources.tracker === "ok"
              ? "記録されたやり取りはまだありません。"
              : "トラッカーが読めないため不明です。"}
          </p>
        ) : (
          <ol className="border-l-2 border-solid-gray-300 pl-4 flex flex-col gap-3">
            {detail.timeline.map((event) => (
              <TimelineItem key={event.comment_id ?? event.at} event={event} />
            ))}
          </ol>
        )}
      </div>
    </div>
  );
}

function TimelineItem({ event }: { event: TimelineEvent }) {
  const [label, palette] = timelineLabel(event);
  return (
    <li>
      <div className="flex items-baseline gap-3">
        <span className={`inline-block px-2 py-0.5 rounded-4 text-std-14B-170 ${palette}`}>
          {label}
        </span>
        <span className="text-std-14N-170 text-solid-gray-536">{formatTime(event.at)}</span>
        {event.url && (
          <a className="text-std-14N-170 text-blue-900 underline" href={event.url} target="_blank" rel="noreferrer">
            コメントを開く
          </a>
        )}
      </div>
      <p className="text-std-14N-170 text-solid-gray-700 whitespace-pre-wrap break-all mt-1">
        {event.kind === "question" ? firstLines(event.body, 14) : firstLines(event.body, 3)}
      </p>
    </li>
  );
}

function timelineLabel(event: TimelineEvent): [string, string] {
  switch (event.kind) {
    case "terminal":
      return event.code === "success"
        ? ["完了", "bg-green-100 text-success-1"]
        : [`失敗 (${event.code ?? "不明"})`, "bg-red-100 text-error-1"];
    case "question":
      return ["質問", "bg-yellow-100 text-solid-gray-800"];
    case "answer":
      return ["回答", "bg-blue-100 text-blue-1000"];
    case "cancel":
      return ["中止", "bg-red-100 text-error-1"];
    case "answer_received":
      return ["回答受領", "bg-blue-50 text-blue-900"];
    case "reception":
      return ["受付", "bg-solid-gray-100 text-solid-gray-700"];
    default:
      return ["メモ", "bg-solid-gray-100 text-solid-gray-700"];
  }
}

function JobMark({ job }: { job: JobNode }) {
  let palette = "bg-solid-gray-300";
  if (job.status === "in_progress") palette = "bg-blue-900 animate-pulse";
  else if (job.conclusion === "success") palette = "bg-success-1";
  else if (job.conclusion === "failure") palette = "bg-error-1";
  else if (job.conclusion === "skipped") palette = "bg-solid-gray-300";
  return <span aria-hidden className={`inline-block w-3 h-3 rounded-full ${palette}`} />;
}

function jobLabel(name: string): string {
  const table: Array<[RegExp, string]> = [
    [/question-tick/, "質問と受付の照合"],
    [/pull-ticket/, "受付と取り込み"],
    [/contract/, "設定の検査"],
    [/tools/, "道具の準備と検査"],
    [/intake/, "チケットの受け入れ"],
    [/source/, "原本の取得"],
    [/model-preflight/, "モデル疎通確認"],
    [/model$/, "判定・実装・レビュー"],
    [/apply|validate/, "変更の適用と機械検査"],
    [/release/, "納品"],
    [/report/, "最終報告"],
  ];
  for (const [pattern, label] of table) if (pattern.test(name)) return label;
  return name;
}

function jobStateLabel(job: JobNode): string {
  if (job.status === "in_progress") return "実行中";
  if (job.status === "queued") return "待機中";
  if (job.conclusion === "success") return "成功";
  if (job.conclusion === "failure") return "失敗";
  if (job.conclusion === "skipped") return "対象外";
  if (job.conclusion === "cancelled") return "取り消し";
  return job.conclusion || job.status || "不明";
}

function formatTime(value?: string): string {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString("ja-JP", { hour12: false });
}

function firstLines(text: string, count: number): string {
  const lines = text.split("\n");
  if (lines.length <= count) return text;
  return lines.slice(0, count).join("\n") + " …";
}
