# Retained-history measurement

Hypershell [run 35202114784](https://github.com/jsell-rh/hypershell-stego/actions/runs/35202114784)
passed at source `dd8529e33dd1b8fb32989e96a53956607f190eb5`, with compiler
`00573709fb15a2a54de4242aa8fdbabee325179a`. It measures the generated SQL cursor
through the application's authorization and transaction path. The fixture has
10,000 or 100,000 synthetic grants, with 90 percent deleted, plus the owner grant.
Each complete scan checks every ordered grant and user identity.

All six samples passed. Each sample has three timed scans. The median complete
scan took 0.1028 seconds for 10,001 references and 1.0207 seconds for 100,001
references. Each page has at most 100 references. The observed ranges were
0.1017–0.1029 seconds and 1.0085–1.0333 seconds. These sizes show approximately
proportional scan time in this fixture; other sizes and concurrent load remain
unmeasured.

The test process had one CPU, a 768 MiB memory limit, and a 128-process limit.
Its root was read-only, it used UID 65532, all capabilities were removed, and
privilege escalation was disabled. PostgreSQL had one CPU and 512 MiB. The test
used the explicit literal-loopback database TLS exception. It ran on hosted CI,
not on the developer workstation or the jshell cluster.

Maximum process RSS was 46,186,496 bytes, about 44.05 MiB, including setup and
prior cases. Median cumulative allocation was 15,004,778 bytes for the smaller
scan and 149,765,877 bytes for the larger scan. Cumulative allocation can exceed
RSS because the garbage collector can reuse memory during a scan.

Independent checks matched the exact source archive, executable hash, compiler
pin, toolchain version, result metrics, container limits, and terminal state.
The downloaded executable was not run locally. CI verified container removal;
there was no independent operator inspection of the hosted runner after cleanup.
See the [consumer evidence](https://github.com/jsell-rh/hypershell-stego/blob/53b511d/acceptance/retained-history-costs.md)
for hashes and the measurement contract. The qualified check is on the consumer
working branch while its earlier main application workflows finish.

This is a SQL inventory baseline. Full account and journal cleanup, provider
calls, transport, controller recovery, and concurrent load remain separate work.
It does not establish a production SLO or close H3 or C6.
