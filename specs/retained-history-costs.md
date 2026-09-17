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

## Account and protected-journal cleanup

Hypershell [run 35203107357](https://github.com/jsell-rh/hypershell-stego/actions/runs/35203107357)
passed at `aade64b9132e7b7d136871cf87f3a23918177409`, with the same compiler.
The fixture has 1,000 active account rows and 1,000 additional provider clients.
All 2,000 clients have encrypted cleanup journals prepared through the common
Keycloak lifecycle. The measured path uses generated SQL, saved scan progress,
protected state, the common client over verified HTTPS, deletion confirmation,
account audit transactions, final inventory, and scope closure.

Each of three samples closed all 1,000 accounts, deleted all 2,000 clients,
and wrote exactly 1,000 success audits in 30 scan cycles. Every protected
journal loaded with valid encryption and closure intent. Registration was sealed.
An unrelated client remained intact. The store and client were reconstructed
after the first 100 accounts; the next page reached the next 100 accounts.
This proves object reconstruction, not a process or database-server restart.

Median timed cleanup was 4.3863 seconds, with a range of 4.0940–4.4134 seconds.
Maximum process RSS was 51,240,960 bytes, about 48.87 MiB. The test retained the
same hard container limits as the grant scan. A repeat of all six grant-scan
samples also passed. Both jobs built identical executable bytes. Independent
checks verified source, compiler, toolchain, result, limits, and terminal state.
The downloaded executables were not run locally. CI verified container removal;
there was no independent operator inspection of the hosted runners afterward.
See the [consumer record](https://github.com/jsell-rh/hypershell-stego/blob/df164f6/acceptance/cleanup-costs.md)
for exact hashes and limits. The qualified branch will follow the existing
main application checks.

The Keycloak endpoint is a controlled HTTPS protocol fixture. Real Keycloak
capacity, previously unknown provider clients, concurrent load, REST and gRPC,
other Gateway controllers, and production SLOs remain open. No generated runtime
change was needed for this fixture. H3 and C6 remain active.
