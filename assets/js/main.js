(function ($) {
	// minDateTime is added to filterout dates which are lesser than this while showing last updated.
	const minDateTime = (new Date("2018/08/01")).getTime();
	const pollInterval = 3000;
	let clipboardBind = false;

	function getParameterByName(name, url) {
		if (!url) url = window.location.href;
		name = name.replace(/[\[\]]/g, "\\$&");
		var regex = new RegExp("[?&]" + name + "(=([^&#]*)|&|#|$)"),
			results = regex.exec(url);
		if (!results) return null;
		if (!results[2]) return '';
		return decodeURIComponent(results[2].replace(/\+/g, " "));
	}

	function clipboard() {
		if (!window.ClipboardJS || clipboardBind) {
			return;
		}
		clipboardBind = true;
		new ClipboardJS('.copy');
	}

	function showCoverage(data) {
		if (!data.Repo) {
			return;
		}

		const url = ["", data.Repo + ".svg?style=flat&tag=" + data.Tag + "&d="].join("/");
		$("#badge").attr("src", url + (new Date()).getTime());
		if (data.AddedAt) {
			const d = new Date(data.AddedAt);
			if (d.getTime() > minDateTime) {
				$("#addedat").text("(last updated: " + d.toLocaleString() + ")");
			}
		}

		const mdurl = ["https://gocover.run", data.Repo + ".svg?style=flat&tag=" + data.Tag].join("/");

		const params = jQuery.param({
			tag: data.Tag,
			repo: data.Repo
		});

		const bdg = "[![gocover.run](" + mdurl + ")](https://gocover.run?" + params + ")";

		$("#mdbadge").text(bdg)
		$("#details").text(data.Cover)

		if (!clipboardBind) {
			$("#coverage").fadeIn();
			clipboard();
		}
	}

	function getCoverage(repo, tag) {
		if (!repo) {
			return;
		}

		const ldom = $("#loading");
		ldom.attr("class", "inline-block");

		$.getJSON({
			url: "/" + repo + ".json?tag=" + tag,
			success: function (body) {
				ldom.attr("class", "hidden");
				showCoverage(body);
				if (body.Cover.indexOf("queued") > -1 || body.Cover.indexOf("progress") > -1) {
					pollStatus(repo, tag);
				}
			},
			error: function () {
				ldom.attr("class", "hidden");
			},
		});
	}

	function pollStatus(repo, tag) {
		if (!repo) {
			return;
		}
		const bdom = $("#badgeloading");
		bdom.attr("class", "inline-block");

		$.getJSON({
			url: "/" + repo + ".json?tag=" + tag,
			success: function (body) {
				if (!body.Cover) {
					return;
				}

				if (body.Cover.indexOf("queued") == -1 && body.Cover.indexOf("progress") == -1) {
					bdom.attr("class", "hidden");
					showCoverage(body);
					return;
				}

				if ($("#details").text() != body.Cover) {
					showCoverage(body);
				}

				window.setTimeout(function () {
					pollStatus(repo, tag);
				}, pollInterval);
			},
			error: function () {
				bdom.attr("class", "hidden");
			},
		});
	}

	$(document).ready(function () {
		var repo = getParameterByName("repo").trim();
		var tag = getParameterByName("tag").trim();
		if (!repo) {
			repo = $("#repo").val().trim();
		}

		if (repo) {
			if (!tag) {
				tag = $("#tag").val().trim();
			}
			$("#repo").val(repo);
			$("#tag").val(tag);
			getCoverage(repo, tag);
		}

		$("form").submit(function (e) {
			if (!$("#repo").val().trim()) {
				e.preventDefault();
			}
		});
	});
})($);