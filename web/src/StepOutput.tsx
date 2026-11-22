import { useRef } from "react";
import { ApiError, type Get, type Graph, type Step, type StepOutput } from "./api";
import { Block, Status, Time } from "./format";
import { Link, useTitle } from "./nav";
import { usePoll } from "./poll";

// A step's gate output, kept by the server after its sandbox is gone (docs/api.md#gate-output).

type Read = { step?: Step; info?: StepOutput; text: string };

// stepOutputPath is the page of a step's gate output.
export function stepOutputPath(itemID: string, stepID: string): string {
  return `/work/${encodeURIComponent(itemID)}/steps/${encodeURIComponent(stepID)}/output`;
}

// readOutput polls the item's graph, which is small, and fetches the output, up to 5 MiB, only
// when a gate run stored a new one.
async function readOutput(last: Read, id: string, stepID: string, get: Get, signal: AbortSignal) {
  const graph = await get<Graph>(`/work/${encodeURIComponent(id)}/graph`, signal);
  const step = graph.steps.find((s) => s.id === stepID);
  const info = graph.step_outputs.find((o) => o.step_id === stepID);
  let text = "";
  if (info && last.info?.created_at === info.created_at) text = last.text;
  else if (info) text = await get<string>(`/steps/${encodeURIComponent(stepID)}/output`, signal);
  return { step, info, text };
}

export function StepOutputPage({ id, stepID }: { id: string; stepID: string }) {
  const last = useRef<Read>({ text: "" });
  const poll = usePoll(`output/${id}/${stepID}`, async (get, signal) => {
    last.current = await readOutput(last.current, id, stepID, get, signal);
    return last.current;
  });
  const { data, error } = poll;
  const missing = !data && error instanceof ApiError && error.status === 404;
  const key = data?.step?.key;
  useTitle(key ? `Gate output of ${key}` : "Gate output");
  return (
    <>
      <h1 tabIndex={-1}>{key ? `Gate output of ${key}` : "Gate output"}</h1>
      <p>
        <Link to={`/work/${encodeURIComponent(id)}`}>Back to the work item</Link>
      </p>
      <Status {...poll} />
      {missing ? (
        <p>
          Nothing to show: the work item does not exist or is not planned, or the output was deleted
          while this page loaded it. Open the work item to find the step again.
        </p>
      ) : null}
      {data && !data.step ? (
        <p>
          The item&apos;s latest plan has no step with the ID <code>{stepID}</code>. It may have
          been replanned; open the work item to find the step again.
        </p>
      ) : data && !data.info ? (
        <p>
          No gate output is kept for this step. It has not run a gate yet, or its output was deleted
          30 days after it was stored.
        </p>
      ) : data?.info ? (
        <>
          <p className="meta">
            <span className="number">{data.info.bytes.toLocaleString()}</span> bytes · stored{" "}
            <Time at={data.info.created_at} />
          </p>
          <p>
            This is the whole output of the step&apos;s latest gate run. Output larger than 5 MiB
            keeps its first and last lines, with a line that says how much was cut.
          </p>
          <Block className="log" label={`Gate output of ${key ?? stepID}`}>
            {data.text}
          </Block>
        </>
      ) : null}
    </>
  );
}
