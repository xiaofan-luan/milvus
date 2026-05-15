# Contributing to Milvus Design Docs

Thank you for your interest in contributing to Milvus design documentation!

## MEP Process

MEP (Milvus Enhancement Proposal) is the process for proposing major changes to Milvus. Here's how it works:

### 1. Before Writing a MEP

- Check existing MEPs to avoid duplicates
- For small changes, a GitHub issue may be sufficient
- Discuss your idea in the community (Slack, GitHub Discussions) first

### 2. Creating a MEP

1. **Copy the template:** Use [MEP-TEMPLATE.md](MEP-TEMPLATE.md) as your starting point
2. **Name your file:** `YYYYMMDD-short-descriptive-name.md` (e.g., `20260128-vector-compression.md`)
3. **Place the file:** Put it in the `design_docs/` directory
4. **Fill in all sections:** At minimum, complete Summary, Motivation, Design Details, and Test Plan

### 3. MEP Status Lifecycle

| Status | Description |
|--------|-------------|
| **Draft** | Initial proposal, open for feedback |
| **Under Review** | Actively being reviewed by maintainers |
| **Approved** | Accepted, ready for implementation |
| **Implemented** | Feature has been merged into Milvus |
| **Deprecated** | No longer applicable or superseded |

### 4. Submitting Your MEP

1. Fork this repository
2. Create your MEP file following the naming convention
3. Submit a Pull Request
4. Address feedback from reviewers
5. Once approved, your MEP will be merged

## File Naming Convention

- **MEP documents:** `YYYYMMDD-descriptive-name.md`
- **Images/diagrams:** Place in `assets/images/` or `assets/graphs/`
- **Reference images in docs:** `![description](../assets/images/your-image.png)`

## Style Guidelines

- Write in clear, concise English
- Use diagrams to explain complex architectures
- Include code examples where helpful
- Keep the scope focused - one MEP per feature

## Questions?

- Open a GitHub issue for questions about the MEP process
- Join the [Milvus Slack](https://slack.milvus.io/) for discussions
