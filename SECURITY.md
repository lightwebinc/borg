# Security

Please report a vulnerability privately through GitHub: the repository's
**Security** tab, **Report a vulnerability**. Do not open a public issue for
it.

A report is most useful with the version (`borg -version`), what you ran,
what you expected, and what happened instead. We aim to acknowledge within
three working days.


In scope:

- anything that lets a reader accept a certificate, grant or group token
  that the organization did not issue, or that no mined anchor commits;
- anything that lets a member removed from a group obtain the key of any
  epoch that began after the removal;
- anything that lets a host admit or serve a record the rules in
  [docs/spec.md](docs/spec.md) refuse;
- anything that leaks an organization's or member's keys or spends coin
  other than as its owner asked;
- the images and the host module bundle.
