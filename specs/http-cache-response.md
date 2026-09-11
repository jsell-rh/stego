The common generated HTTPS client must preserve a 304 Not Modified response
to GET or HEAD. This response completes a cache check. It does not instruct the
client to send a new request.

The previous client rejected every response from 300 through 399. The browser
backend exposed this contract error through its conditional API proxy.
The correction returns status 304, response headers, and the empty body.
Redirect responses remain errors, and the client does not follow Location.
A 304 response to a mutation remains an error.

The correction applies to go-sdk 2.0.2, http-application 1.5.2, and
cli-application 1.6.2. The generated transport test uses HTTPS. It checks GET
and HEAD cache responses, rejected redirects, no request to the redirect
target, and a rejected mutation response. The browser runtime test checks
that status 304 and ETag pass through its API proxy.

The bounded cluster check passed under race detection. The generated client
package took 2.584 seconds. The generated browser runtime passed its proxy
check as part of a 3.130-second runtime suite. Registry checks took 2.212
seconds. Source and results are in `/tmp/stego-browser-runtime-gn_86o_n` on
the test workstation.

Hypershell commit `cf21ebd` pins compiler `79c006c` and adopts the three
component versions. Two cluster generation passes and the post-test output
had the same 129 generated, state, and dependency hashes. The checkout
matched them. The input-manifest race test passed in 1.053 seconds. The Job
completed. Full CI for the new revisions remains a separate check.
This change does not add automatic response caching.
