# Contribution Guide

## 1. Overview
We manage our codebase on GitHub. Read the [GitHub Pull Request Guide](https://docs.github.com/en/github/collaborating-with-issues-and-pull-requests/about-pull-requests) before you start.

---

## 2. Branch Organization
We store stable production code in the `main` branch. We integrate active development into the `development` branch. When we make breaking API changes, we create a new package directory with a version suffix (`v2` or `v3`).

We use the [Git Flow](https://nvie.com/posts/a-successful-git-branching-model/) model. Use one of these prefixes when you create a branch name:
- `feature/`
- `bugfix/`
- `refactor/`
- `test/`
- `doc/`
- `ci/`
- `optimize/`

---

## 3. Reporting Issues

### 3.1 Known Issues
Search [GitHub Issues](https://github.com/Jkenyut/nvx-go-client/issues) before you open a new issue. Verify that another contributor has not reported your problem already.

### 3.2 Bug Reports
Provide a minimal code sample that reproduces the failure. You can attach code directly to the issue or link to the [Go Playground](https://play.golang.org/).

### 3.3 Security Vulnerabilities
Do not post security vulnerabilities in public issues. Send vulnerability disclosures to our [Security Team](mailto:satrianursaputro06@gmail.com).

---

## 4. Pull Request Procedure

Follow these procedural steps to submit a pull request:

1. Search open and closed pull requests on [GitHub](https://github.com/Jkenyut/nvx-go-client/pulls) to avoid duplicate submissions.
2. Open an issue to describe the defect or to propose the feature design.
3. [Fork](https://docs.github.com/en/github/getting-started-with-github/fork-a-repo) the repository to your GitHub account.
4. Create a new branch from `development`:
   ```bash
   git checkout -b bugfix/issue-description development
   ```
5. Implement your code changes.
6. Write unit tests that cover your modifications.
7. Format the code with `gofmt`:
   ```bash
   gofmt -w .
   ```
8. Run the static code analysis tool:
   ```bash
   go vet ./...
   ```
9. Run all unit tests with race detection:
   ```bash
   go test -v -race ./...
   ```
10. Commit your changes using [Conventional Commits](https://www.conventionalcommits.org/).
11. Push your branch to your forked repository:
    ```bash
    git push origin bugfix/issue-description
    ```
12. Submit a pull request targeted at the `development` branch.

---

## 5. Coding Standards

Follow the standard [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments).

Useful references:
- [Effective Go](https://golang.org/doc/effective_go)
- [Uber Go Style Guide](https://github.com/uber-go/guide/blob/master/style.md)