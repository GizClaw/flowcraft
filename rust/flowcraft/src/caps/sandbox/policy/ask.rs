//! Asking a configured program, with a deadline and a JSON line either way.
//!
//! The sandbox has two questions it may need to ask someone — "may this run?"
//! ([`super::approval::Approver`]) and "may this one command leave the confine?"
//! ([`super::escalation::Escalator`]) — and both are the same mechanism: run the
//! program the deployment named, hand it a JSON line on stdin, read a JSON line on
//! stdout, and give up after the deadline. One implementation, so the two answers
//! cannot drift apart in how they time out or how they fail.
//!
//! Why a program and not an HTTP endpoint: a program composes with whatever the
//! operator already has (a terminal prompt, a CI gate, a chat webhook wrapped in a
//! script), needs no client library, and is testable with a shell script. The
//! request travels on **stdin**, never in argv, so a command line can never end up
//! in a process listing or in a shell.
//!
//! Failure is closed by construction: a non-zero exit, a deadline, a missing
//! program, or output that is not the expected JSON is an `Err`, and both callers
//! treat `Err` as "no".

use std::process::Stdio;
use std::time::Duration;

use serde::Serialize;
use serde::de::DeserializeOwned;

use crate::error::{Error, Result};

/// How long one question may take. Long enough for a human to answer, short
/// enough that a wedged prompt does not hold a start forever.
pub const DEFAULT_TIMEOUT: Duration = Duration::from_secs(60);

/// A program that answers questions.
#[derive(Debug, Clone)]
pub struct Ask {
    argv: Vec<String>,
    timeout: Duration,
}

impl Ask {
    /// A questioner that runs `argv` and waits at most `timeout`.
    pub fn new(argv: Vec<String>, timeout: Duration) -> Result<Self> {
        if argv.is_empty() || argv[0].trim().is_empty() {
            return Err(Error::validation(
                "sandbox: an approver command must not be empty",
            ));
        }
        if timeout.is_zero() {
            return Err(Error::validation(
                "sandbox: an approver timeout must be positive",
            ));
        }
        Ok(Self { argv, timeout })
    }

    /// The configured command line, for a `Debug` print that redacts nothing (it is
    /// a path, not a secret).
    #[must_use]
    pub fn argv(&self) -> &[String] {
        &self.argv
    }

    /// Ask: one JSON line in, one JSON line out, or an error.
    ///
    /// The answer is parsed from the **last** non-empty line of stdout, so a script
    /// may print a prompt or a log first without breaking the contract.
    pub async fn ask<T, A>(&self, request: &T) -> Result<A>
    where
        T: Serialize + Sync,
        A: DeserializeOwned,
    {
        use tokio::io::AsyncWriteExt as _;

        let mut command = tokio::process::Command::new(&self.argv[0]);
        command
            .args(&self.argv[1..])
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            // A questioner that forks its own children must not outlive the answer.
            .kill_on_drop(true);
        let mut child = command.spawn().map_err(|error| {
            Error::not_available(format!(
                "sandbox: cannot run the approver {:?}: {error}",
                self.argv[0]
            ))
        })?;
        if let Some(mut stdin) = child.stdin.take() {
            let mut line = serde_json::to_vec(request).map_err(|error| {
                Error::internal(format!("sandbox: encode the request: {error}"))
            })?;
            line.push(b'\n');
            stdin.write_all(&line).await.map_err(|error| {
                Error::internal(format!("sandbox: write to the approver: {error}"))
            })?;
            // Closing the input is what tells a `read`-then-answer script to go.
            drop(stdin);
        }
        let output = tokio::time::timeout(self.timeout, child.wait_with_output())
            .await
            .map_err(|_| {
                Error::timeout(format!(
                    "sandbox: the approver did not answer within {:?}",
                    self.timeout
                ))
            })?
            .map_err(|error| Error::internal(format!("sandbox: the approver failed: {error}")))?;
        if !output.status.success() {
            return Err(Error::policy_denied(format!(
                "sandbox: the approver exited with {}",
                output.status
            )));
        }
        let text = String::from_utf8_lossy(&output.stdout);
        let answer = text
            .lines()
            .rev()
            .map(str::trim)
            .find(|line| !line.is_empty())
            .ok_or_else(|| Error::policy_denied("sandbox: the approver said nothing"))?;
        serde_json::from_str(answer).map_err(|error| {
            Error::policy_denied(format!(
                "sandbox: the approver's answer is not the expected JSON: {error}"
            ))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde::Deserialize;

    #[derive(Debug, Serialize)]
    struct Request {
        command: String,
    }

    #[derive(Debug, Deserialize, PartialEq)]
    struct Answer {
        allow: bool,
    }

    fn ask(script: &str, timeout: Duration) -> Ask {
        // `sh -c <script>`: the smallest questioner there is, and it exercises the
        // real fork/exec path rather than a mock.
        Ask::new(
            vec!["sh".to_owned(), "-c".to_owned(), script.to_owned()],
            timeout,
        )
        .expect("ask")
    }

    fn request() -> Request {
        Request {
            command: "git status".to_owned(),
        }
    }

    #[tokio::test]
    async fn the_request_arrives_on_stdin_and_the_answer_on_stdout() {
        // Echo the request back as a JSON object the caller can parse: proof that
        // stdin carried it, and that only the last line is read.
        let asker = ask(
            "cat >/dev/null; echo 'not json, ignore me'; echo '{\"allow\": true}'",
            DEFAULT_TIMEOUT,
        );
        assert_eq!(
            asker.ask::<_, Answer>(&request()).await.expect("answer"),
            Answer { allow: true }
        );
    }

    #[tokio::test]
    async fn a_failure_or_a_deadline_is_an_error_not_an_answer() {
        // Non-zero exit.
        let asker = ask("cat >/dev/null; exit 3", DEFAULT_TIMEOUT);
        assert!(asker.ask::<_, Answer>(&request()).await.is_err());

        // No output at all.
        let asker = ask("cat >/dev/null", DEFAULT_TIMEOUT);
        assert!(asker.ask::<_, Answer>(&request()).await.is_err());

        // Not the expected JSON.
        let asker = ask("cat >/dev/null; echo yes", DEFAULT_TIMEOUT);
        assert!(asker.ask::<_, Answer>(&request()).await.is_err());

        // Wedged: the deadline fires instead of holding the start forever.
        let asker = ask("cat >/dev/null; sleep 30", Duration::from_millis(100));
        let error = asker
            .ask::<_, Answer>(&request())
            .await
            .expect_err("timeout");
        assert_eq!(error.kind(), crate::error::ErrorKind::Timeout);
    }

    #[tokio::test]
    async fn a_missing_program_is_reported_rather_than_guessed() {
        let asker =
            Ask::new(vec!["/nonexistent/approver".to_owned()], DEFAULT_TIMEOUT).expect("ask");
        let error = asker
            .ask::<_, Answer>(&request())
            .await
            .expect_err("no program");
        assert_eq!(error.kind(), crate::error::ErrorKind::NotAvailable);
    }
}
