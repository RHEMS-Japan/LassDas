import { useMemo, useState } from "react";

type Choice = { line: string; letter: string; label: string };
type Question = { id: string; text: string; choices: Choice[] };

// The open question is answerable right here: the panel offers exactly the
// answer lines the question itself printed, one choice per question, with a
// confirmation step before the one-and-only send. Free-form answers are a
// later phase that needs the engine's consent, not the console's.
export function AnswerPanel({
  issueKey,
  commentID,
  body,
  onAnswered,
}: {
  issueKey: string;
  commentID: string;
  body: string;
  onAnswered: () => void;
}) {
  const questions = useMemo(() => parseQuestions(body), [body]);
  const [selection, setSelection] = useState<Record<string, string>>({});
  const [confirming, setConfirming] = useState(false);
  const [sending, setSending] = useState(false);
  const [error, setError] = useState("");
  const [posted, setPosted] = useState("");

  if (questions.length === 0) return null;
  const complete = questions.every((question) => selection[question.id]);
  const lines = questions.map((question) => selection[question.id]).filter(Boolean);

  const send = async () => {
    setSending(true);
    setError("");
    try {
      const response = await fetch(`/api/tickets/${encodeURIComponent(issueKey)}/answer`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ question_comment_id: commentID, lines }),
      });
      if (!response.ok) throw new Error(await response.text());
      const result = (await response.json()) as { posted_comment_id: string };
      setPosted(result.posted_comment_id);
      onAnswered();
    } catch (cause) {
      setError(String(cause));
    } finally {
      setSending(false);
    }
  };

  if (posted) {
    return (
      <div className="border border-success-2 bg-green-50 rounded-8 p-4">
        <p className="text-std-16B-170 text-success-1">回答を投稿しました (コメント #{posted})</p>
        <p className="text-std-14N-170 text-solid-gray-700">
          まもなく自動処理が回答を取り込みます (通常は数秒、最長で数分)。この画面は 30 秒ごとに更新されます。
        </p>
      </div>
    );
  }

  return (
    <div className="border border-yellow-500 bg-yellow-50 rounded-8 p-4 flex flex-col gap-4">
      <p className="text-std-16B-170 text-solid-gray-900">この質問に回答できます</p>
      {questions.map((question) => (
        <fieldset key={question.id} className="flex flex-col gap-2">
          <legend className="text-std-16B-170 text-solid-gray-900 mb-1">{question.text}</legend>
          {question.choices.map((choice) => (
            <label
              key={choice.line}
              className={`flex gap-3 items-start border rounded-8 px-4 py-3 bg-white cursor-pointer ${
                selection[question.id] === choice.line
                  ? "border-blue-900 ring-1 ring-blue-900"
                  : "border-solid-gray-300"
              }`}
            >
              <input
                type="radio"
                name={question.id}
                className="mt-1"
                checked={selection[question.id] === choice.line}
                onChange={() =>
                  setSelection((current) => ({ ...current, [question.id]: choice.line }))
                }
              />
              <span className="text-std-16N-170 text-solid-gray-800">
                <span className="text-std-16B-170">{choice.letter}: </span>
                {choice.label}
              </span>
            </label>
          ))}
        </fieldset>
      ))}

      {!confirming ? (
        <button
          type="button"
          disabled={!complete}
          onClick={() => setConfirming(true)}
          className={`self-start px-6 py-2 rounded-8 text-std-16B-170 ${
            complete
              ? "bg-blue-900 text-white hover:bg-blue-1000"
              : "bg-solid-gray-200 text-solid-gray-536 cursor-not-allowed"
          }`}
        >
          回答内容を確認する
        </button>
      ) : (
        <div className="border border-solid-gray-300 bg-white rounded-8 p-4 flex flex-col gap-3">
          <p className="text-std-16B-170 text-solid-gray-900">この内容で投稿します (取り消せません)</p>
          <ul className="flex flex-col gap-1">
            {lines.map((line) => (
              <li key={line} className="text-std-16N-170 font-mono text-solid-gray-800">
                {line}
              </li>
            ))}
          </ul>
          <div className="flex gap-3">
            <button
              type="button"
              disabled={sending}
              onClick={send}
              className="px-6 py-2 rounded-8 text-std-16B-170 bg-blue-900 text-white hover:bg-blue-1000 disabled:bg-solid-gray-200 disabled:text-solid-gray-536"
            >
              {sending ? "送信中…" : "投稿する"}
            </button>
            <button
              type="button"
              disabled={sending}
              onClick={() => setConfirming(false)}
              className="px-6 py-2 rounded-8 text-std-16B-170 border border-solid-gray-400 text-solid-gray-800 bg-white"
            >
              選び直す
            </button>
          </div>
        </div>
      )}
      {error && (
        <p className="text-std-14N-170 text-error-1 break-all" role="alert">
          投稿できませんでした: {error}
        </p>
      )}
    </div>
  );
}

// parseQuestions extracts, from the question comment's own text, each
// question with the exact answer lines it printed. Anything the parser
// cannot pair is simply not offered - the Backlog comment remains the
// fallback for every case the panel cannot express.
function parseQuestions(body: string): Question[] {
  const lines = body.split("\n");
  const questions: Question[] = [];
  let current: Question | null = null;
  let lastChoiceLabel = "";
  for (const raw of lines) {
    const line = raw.trim();
    const questionHead = line.match(/^([Qq]\d+)\.\s*(.+)$/);
    if (questionHead) {
      current = { id: questionHead[1], text: `${questionHead[1]}. ${questionHead[2]}`, choices: [] };
      questions.push(current);
      lastChoiceLabel = "";
      continue;
    }
    const choiceHead = line.match(/^-\s*(\S+):\s*(.+)$/);
    if (choiceHead) {
      lastChoiceLabel = choiceHead[2];
      continue;
    }
    const answerLine = line.match(/^回答 C\d+ (\S+):(\S+)$/);
    if (answerLine && current && current.id === answerLine[1] && lastChoiceLabel) {
      current.choices.push({ line, letter: answerLine[2], label: lastChoiceLabel });
      lastChoiceLabel = "";
    }
  }
  return questions.filter((question) => question.choices.length >= 2);
}
