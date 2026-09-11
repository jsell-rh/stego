# Kafka Secret files

The generated Kafka client permits read-only group access to private files.
This permits a non-root Kubernetes process to read projected Secret files with
mode `0440` and an assigned file group. Modes `0400`, `0600`, `0440`, and `0640`
are permitted. Execute bits, group-write access, and other access are rejected.
The same rule applies to the mutual-TLS private key and the SCRAM password file.

Inputs must be regular files. Projected links are permitted. The reader checks
that the file identity and mode did not change between the initial check and
the open descriptor. Reads stop after 65,536 bytes. Use local mounted files;
file-system operations do not have a wall-clock deadline.

The Hypershell Kubernetes workflow exposed this gap: the generated deployment
mounted private files with mode `0440`, but the Kafka client required owner-only
access. The fix belongs in STEGO. The variant does not copy or change the mode
of mounted credentials.

The generated publisher test now sends and receives a record with a projected
`0440` key. It retains mutual TLS, all-replica acknowledgements, and event identity
checks. Configuration tests check accepted modes and reject unsafe modes before
network access. The bounded jshell test passed with PostgreSQL required and race
detection on 2026-09-11. The generator package took 12.194 seconds.
