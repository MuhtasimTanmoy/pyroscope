// Note: Bind this to the component that calls it

export function buildRenderURL() {
  let width = document.body.clientWidth - 30;
  let { from, until } = this.props;
  
  let url = `/render?from=${encodeURIComponent(from)}&until=${encodeURIComponent(until)}&width=${width}`;
  let nameLabel = this.props.labels.find(x => x.name == "__name__");

  if (nameLabel) {
    url += "&name=" + nameLabel.value + "{";
  } else {
    url += "&name=unknown{";
  }

  // TODO: replace this so this is a real utility function
  url += this.props.labels.filter(x => x.name != "__name__").map(x => `${x.name}=${x.value}`).join(",");
  url += "}";

  if (this.props.refreshToken) {
    url += `&refreshToken=${this.props.refreshToken}`
  }
  url += `&max-nodes=${this.props.maxNodes}`

  return url;
}

// Note: Bind this to the component that calls it

export function fetchJSON(url) {
  console.log('fetching json: url: ', url);

  let formattedUrl = url + '&format=json'

  console.log('fetching json: formattedURl', formattedUrl);
  if (this.currentJSONController) {
    this.currentJSONController.abort();
  }

  this.currentJSONController = new AbortController();
  fetch(formattedUrl, { signal: this.currentJSONController.signal })
    .then((response) => {
      return response.json()
    })
    .then((data) => {
      this.props.actions.receiveJSON(data)
    })
    .finally();
}

export function fetchNames() {
  if (this.currentNamesController) {
    this.currentNamesController.abort();
  }
  this.currentNamesController = new AbortController();
  // dispatch(requestNames());
  console.log('fetching names');

  fetch("/label-values?label=__name__", { signal: this.currentNamesController.signal })
    .then((response) => {
      return response.json()
    })
    .then((data) => {
      this.props.actions.receiveNames(data)
    })
    .finally()
}


