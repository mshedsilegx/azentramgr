Review, understand and use at all times the general principles below, listed by decreasing level of importance

1- Before coding, think through this step by step and plan the entire application architecture. Always think, use tools as needed, then plan, then communicate the plan for approval. Never code right away.
2- Always optimize for performance by parallelizing as much as possible, and a very large number of objects
3- Make sure are we not flooding the used API's with requests, and that a retry mechanism with exponential backoff is implemented to address any transient errors. Both would have best practice values set by default.
4- Only use well maintained, safe and production ready golang modules. Limit the use of third party libraries if possible, always prefer core libs.
5- IMPORTANT: Do not rely on replace tool for complex, multi-line changes, as it causes repeated failures and endless loops, use write_file
6- Before making any change, backup the existing file with a timestamp suffix
7- Use the following go commands (in this order) to validate the code generated at every step and systematically, before committing: get -u, go mod tidy, go fmt, go vet, golangci-lint, govulncheck. Do not build the binary nor submit to github repo without approval. Do not auto-approve plans.
8- For a complex problem, decompose in smaller units, and validate each before re-assembling into the final solution
9- Use the following standard arguments:
     -version to display the current application version. Defaults to dev, and is overwritten at compile time via -X main.version=<version>
     -silent to run the application with no output
     -config <file> to use a JSON formatted property file to define application parameters
10- After the code is generated, do a deep analysis of the code base produced for recommendations and improvements. They all need to be approved. Perform a code review for security, performance (scalability and parallelism) and modularity, and provide recommendations.
11- Perform a security analysis and review, provide security recommendations. Make sure all inputs are properly sanitized and secured to avoid any malicious injections
12- Provide comprehensive documentation in README.md with section: Application overview and objectives, architecture and design choices, command line arguments (list with description, type and defaults), examples on how to use.
13- Use always the Unix line termination (\n), even if windows is detected
14- Use TZ US/Central for all date operations
15- Do not autocommit to remote git repository. All git push operations must be explicitly requested and confirmed
