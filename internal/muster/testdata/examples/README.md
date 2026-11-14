# Contract v1 examples

One file for each call and each refusal in [`../CONTRACT.md`](../CONTRACT.md). A test replays every file against Muster running on its in-memory stand-in for Logbook, and fails if a call or a refusal has no example.

```sh
go test ./internal/httpapi -run TestContractExamples
```

Every example starts at the same made-up moment, `2026-01-05T09:00:00Z`, with the default settings: a 60-second lease, a 20-bit puzzle and 5 submissions per visitor per day. All data is made up.

## What a file holds

| Field | Meaning |
| --- | --- |
| `about` | What the example shows, in a sentence |
| `given.items` | Made-up items on record before anything happens |
| `given.calls` | Earlier calls that set the scene, in order. Each must succeed |
| `given.wait` | Time that passes after those calls |
| `given.condition` | Something wrong that no request can cause, such as Logbook taking too long |
| `request` | The call: `as`, `method`, `path` and `body` |
| `expect.status` | The HTTP status expected |
| `expect.body` | The exact body expected. Left out when there is none |
| `expect.stream` | For the feed: the events a new subscriber is sent at once |

`as` names the ServiceAccount whose token is presented, in place of the token itself. Where `as` is left out, no token is presented.

Bodies are compared exactly, field for field and in the same order, after white space between fields is removed.

## The files

Files whose names begin `refused-` are the refusals, named by status and cause. The rest are the calls.

To change what Muster does on purpose, change the code, run the test with `-update`, and read the difference. Every changed line is a change in the contract.
